package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// A GraphQL surface, for a front end that already speaks it.
//
// The case for it is embedded analytics: a React application querying its
// own backend, where the team already has a GraphQL gateway and adding a
// REST call means a second client, a second auth path and a second set of
// types. This is smaller than that.
//
// # It is deliberately one query field, not a generated schema
//
// The obvious design is to generate a GraphQL type per metric so a caller
// writes `{ orderRevenue(groupBy: REGION) }`. That is rejected for the
// reason the SQL grammar is small: a schema generated from the model has
// to change shape when the model changes, which breaks every persisted
// query and every generated client on a metric rename, and it invites a
// caller to compose fields into a request the planner would refuse. One
// field taking metric and dimension names keeps the request vocabulary
// identical to REST and MCP, so a refusal means the same thing on all
// three.
//
// # What it is not
//
// Not a general GraphQL server. No mutations, because there is no write
// path. No subscriptions, because there is nothing to subscribe to on this
// endpoint; a refusal notification is a webhook. Introspection is answered
// only far enough for a client to validate the one query it can send,
// because a full introspection response is a promise about a schema that
// is intentionally this small.

// graphQLRequest is the standard POST body.
type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
	Operation string         `json:"operationName"`
}

// graphQLResponse is the standard envelope.
//
// Errors go in `errors` with the refusal's code and hint in `extensions`,
// which is where a GraphQL client looks for machine-readable detail. A
// refusal returned as a 4xx instead would be invisible to every GraphQL
// client, because they read the body and ignore the status.
type graphQLResponse struct {
	Data   map[string]any `json:"data,omitempty"`
	Errors []graphQLError `json:"errors,omitempty"`
}

type graphQLError struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

func (s *Server) graphQL(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeReadModel)
	if !ok {
		return
	}

	var req graphQLRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeGraphQLError(w, "the request body is not valid GraphQL JSON", nil)
		return
	}

	op, err := parseGraphQL(req.Query)
	if err != nil {
		writeGraphQLError(w, err.Error(), nil)
		return
	}

	switch op.field {
	case "__schema", "__type":
		writeJSON(w, http.StatusOK, graphQLResponse{Data: introspectionResponse(op.field)})

	case "metrics":
		writeJSON(w, http.StatusOK, graphQLResponse{
			Data: map[string]any{"metrics": s.graphQLMetrics(id)},
		})

	case "dimensions":
		dims, err := s.engine().VisibleDimensions(r.Context(), id, nil)
		if err != nil {
			writeGraphQLError(w, "listing dimensions failed", nil)
			return
		}
		names := make([]string, 0, len(dims))
		for _, d := range dims {
			names = append(names, d.Qualified)
		}
		writeJSON(w, http.StatusOK, graphQLResponse{
			Data: map[string]any{"dimensions": names},
		})

	case "query":
		s.graphQLQuery(w, r, id, op, req.Variables)

	default:
		writeGraphQLError(w, fmt.Sprintf(
			"unknown field %q. This endpoint serves metrics, dimensions and "+
				"query; the request vocabulary is the same one REST and MCP use, "+
				"so a question phrased here means exactly what it means there",
			op.field), nil)
	}
}

func (s *Server) graphQLMetrics(id govern.Identity) []map[string]any {
	out := []map[string]any{}
	for _, vm := range s.engine().Metrics() {
		out = append(out, map[string]any{
			"name":        vm.Qualified,
			"namespace":   vm.Namespace.Name,
			"description": vm.Metric.Description,
			"datatype":    vm.Metric.Datatype,
		})
	}
	return out
}

func (s *Server) graphQLQuery(w http.ResponseWriter, r *http.Request,
	id govern.Identity, op *graphQLOp, variables map[string]any) {

	if !id.Can(ScopeRunQuery) {
		writeGraphQLError(w, "this credential may read the model but not run a query",
			map[string]any{"code": "insufficient_scope", "retry": "never"})
		return
	}

	req, err := requestFromGraphQL(op, variables)
	if err != nil {
		writeGraphQLError(w, err.Error(), nil)
		return
	}

	res, err := s.engine().Query(r.Context(), id, req)
	if err != nil {
		var refusal *plan.Refusal
		if asRefusalErr(err, &refusal) {
			// The refusal travels as a GraphQL error with its code, hint and
			// retry class in extensions. A client that gave up here without
			// reading the hint would be giving up on a question the engine
			// just told it how to ask.
			writeGraphQLError(w, refusal.Reason, map[string]any{
				"code":  refusal.Code,
				"hint":  refusal.Hint,
				"retry": string(refusal.Retry()),
			})
			return
		}
		writeGraphQLError(w, "the query failed", nil)
		return
	}

	writeJSON(w, http.StatusOK, graphQLResponse{Data: map[string]any{
		"query": map[string]any{
			"columns":      res.Columns,
			"rows":         res.Rows,
			"rowCount":     res.RowCount,
			"modelVersion": res.ModelVersion,
			"dialect":      res.Dialect,
		},
	}})
}

// requestFromGraphQL turns the parsed arguments into a semantic request.
//
// The same plan.Request every other surface builds, which is the point: a
// fan-out refused over REST is refused here, with the same code and the
// same hint, because it is the same planner seeing the same question.
func requestFromGraphQL(op *graphQLOp, variables map[string]any) (plan.Request, error) {
	var req plan.Request

	metrics, err := stringList(op.arg("metrics", variables))
	if err != nil {
		return req, fmt.Errorf("metrics: %w", err)
	}
	if len(metrics) == 0 {
		return req, fmt.Errorf(
			"query needs at least one metric: query(metrics: [\"order_revenue\"])")
	}
	req.Metrics = metrics

	if req.Dimensions, err = stringList(op.arg("dimensions", variables)); err != nil {
		return req, fmt.Errorf("dimensions: %w", err)
	}
	if v := op.arg("limit", variables); v != nil {
		n, ok := asInt(v)
		if !ok {
			return req, fmt.Errorf("limit must be a number")
		}
		req.Limit = n
	}
	if v := op.arg("grain", variables); v != nil {
		if s, ok := v.(string); ok {
			req.Grain = plan.Grain(strings.ToLower(s))
		}
	}
	// Filters arrive as the same structured objects the REST body takes,
	// rather than as a GraphQL expression language. A second filter syntax
	// would be a second thing to get wrong, and this one is already the
	// vocabulary the planner reads.
	if v := op.arg("filters", variables); v != nil {
		raw, err := json.Marshal(v)
		if err != nil {
			return req, fmt.Errorf("filters: %w", err)
		}
		if err := json.Unmarshal(raw, &req.Filters); err != nil {
			return req, fmt.Errorf(
				"filters must be a list of {dimension, op, values}: %w", err)
		}
	}
	return req, nil
}

func stringList(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case []string:
		return t, nil
	case string:
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("expected a list of names")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected a list of names")
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case float64:
		return int(t), true
	case json.Number:
		n, err := t.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func writeGraphQLError(w http.ResponseWriter, message string, extensions map[string]any) {
	// 200 with an errors array, which is what the GraphQL specification
	// says and what every client reads. Returning 4xx would make the
	// refusal invisible to clients that only parse the body.
	writeJSON(w, http.StatusOK, graphQLResponse{
		Errors: []graphQLError{{Message: message, Extensions: extensions}},
	})
}

// introspectionResponse answers enough for a client to validate its query.
//
// Deliberately minimal. A full introspection response is a promise about a
// schema, and this schema is intentionally three fields; generating a type
// per metric would make the promise change every time somebody renames
// one.
func introspectionResponse(field string) map[string]any {
	schema := map[string]any{
		"queryType":    map[string]any{"name": "Query"},
		"mutationType": nil,
		// No subscriptions: there is nothing to subscribe to here, and a
		// refusal notification is a webhook rather than a socket.
		"subscriptionType": nil,
		"types": []any{
			map[string]any{
				"kind": "OBJECT", "name": "Query",
				"description": "The semantic model. One query field, taking the " +
					"same metric and dimension names REST and MCP take.",
				"fields": []any{
					map[string]any{"name": "metrics", "description": "Every metric this caller may read."},
					map[string]any{"name": "dimensions", "description": "Every dimension this caller may read."},
					map[string]any{"name": "query", "description": "Answer a question, or refuse it."},
				},
			},
		},
	}
	if field == "__type" {
		return map[string]any{"__type": schema["types"].([]any)[0]}
	}
	return map[string]any{"__schema": schema}
}

// asRefusalErr binds a structured refusal, so the handler can return the
// code and hint rather than a flattened message.
func asRefusalErr(err error, target **plan.Refusal) bool { return errors.As(err, target) }
