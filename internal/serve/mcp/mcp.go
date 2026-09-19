// Package mcp exposes the engine to agents over the Model Context Protocol.
//
// There is no run_sql tool and there will never be one. An agent cannot express
// an ungoverned query because the request vocabulary does not contain one. That
// is the entire argument for routing an agent through a semantic layer instead
// of pointing it at the warehouse, and it survives exactly as long as the
// escape hatch stays absent. TestNoRawSQLTool guards it.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Version is reported to clients during initialization.
const Version = "0.1.0"

// Server binds an engine to an MCP server.
type Server struct {
	eng *engine.Engine
	// identity is the workload every request from this transport runs as.
	// Over stdio it comes from the process configuration; over HTTP the
	// handler derives it per request from the bearer token.
	identity govern.Identity
}

// New builds an MCP server over an engine.
func New(eng *engine.Engine, id govern.Identity) *Server {
	return &Server{eng: eng, identity: id}
}

// MCPServer returns a configured SDK server with the four tools registered.
func (s *Server) MCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "semantic-" + s.eng.ModelName(),
		Title:   "Semantic layer: " + s.eng.ModelName(),
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: s.instructions(),
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "list_metrics",
		Title: "List metrics",
		Description: "List the metrics this semantic layer can answer, with a plain-language " +
			"description of each. Call this first when you do not already know the exact " +
			"metric name. Use the optional `search` argument to narrow by name, description " +
			"or synonym. Do not guess a metric name: a name that is not in this list does " +
			"not exist and the query will be refused.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.listMetrics)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "describe_metric",
		Title: "Describe a metric",
		Description: "Return the full definition of one metric: what it measures, how it is " +
			"aggregated, and exactly which dimensions it can be grouped by. Call this before " +
			"`query` whenever you are not certain a metric answers the question asked, or " +
			"which dimensions are legal for it. Grouping a metric by a dimension not listed " +
			"here will be refused.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.describeMetric)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "list_dimensions",
		Title: "List dimensions",
		Description: "List the dimensions available for grouping and filtering. Pass `metric` " +
			"to get only the dimensions that are valid for that metric, which is almost always " +
			"what you want. Dimensions are named `dataset.field` and must be given in that form.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.listDimensions)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "query",
		Title: "Run a semantic query",
		Description: "Answer a question by naming metrics, dimensions and filters. This is the " +
			"only way to get numbers, and it is not a SQL interface: there is no way to send " +
			"raw SQL and no way to bypass the model's definitions. The response carries the " +
			"rows, the SQL that was compiled, and the model version, so the number can always " +
			"be traced back. Filters are structured objects, never SQL text. If a request is " +
			"refused, the refusal names what was wrong and what to try instead; read it and " +
			"correct the request rather than retrying it unchanged.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.query)

	return srv
}

func (s *Server) instructions() string {
	h := s.eng.Health()
	var b strings.Builder
	fmt.Fprintf(&b, "This server answers questions about the %q semantic workspace (version %s).\n\n",
		h.Workspace, h.WorkspaceDigest)
	if len(h.Namespaces) > 1 {
		b.WriteString("Metrics are grouped into namespaces owned by different teams and are " +
			"named `namespace.metric`. A single query may only use metrics from one namespace.\n\n")
	}
	b.WriteString("Workflow: list_metrics to find the metric, describe_metric to confirm it " +
		"answers the question and to see its legal dimensions, then query.\n\n")
	b.WriteString("Every number comes from a governed model definition. There is no raw SQL " +
		"interface. If you need something the model does not define, say so rather than " +
		"approximating it with a different metric.\n")
	if len(h.EnforcementNotes) > 0 {
		b.WriteString("\nLimits of this deployment:\n")
		for _, n := range h.EnforcementNotes {
			b.WriteString("  - " + n + "\n")
		}
	}
	return b.String()
}

// ---------- list_metrics ----------

type listMetricsIn struct {
	Search string `json:"search,omitempty" jsonschema:"case-insensitive substring matched against metric name, description and synonyms"`
}

type metricSummary struct {
	Name        string   `json:"name"`
	Namespace   string   `json:"namespace"`
	Description string   `json:"description"`
	Aggregation string   `json:"aggregation"`
	Datatype    string   `json:"datatype,omitempty"`
	Synonyms    []string `json:"synonyms,omitempty"`
	Dimensions  []string `json:"dimensions"`
}

type listMetricsOut struct {
	Model        string          `json:"model"`
	ModelVersion string          `json:"model_version"`
	Metrics      []metricSummary `json:"metrics"`
}

func (s *Server) listMetrics(ctx context.Context, _ *mcp.CallToolRequest, in listMetricsIn) (*mcp.CallToolResult, listMetricsOut, error) {
	out := listMetricsOut{Model: s.eng.ModelName(), ModelVersion: s.eng.ModelVersion()}
	needle := strings.ToLower(strings.TrimSpace(in.Search))

	for _, vm := range s.eng.Metrics() {
		if needle != "" && !metricMatches(vm.Metric, needle) {
			continue
		}
		dims, err := s.eng.VisibleDimensions(ctx, s.identity, &vm)
		if err != nil {
			return nil, out, fmt.Errorf("resolving dimension access: %w", err)
		}
		out.Metrics = append(out.Metrics, metricSummary{
			Name:        vm.Qualified,
			Namespace:   vm.Namespace.Name,
			Description: vm.Metric.Description,
			Aggregation: aggregationOf(vm.Metric),
			Datatype:    vm.Metric.Datatype,
			Synonyms:    vm.Metric.AIContext.Synonyms,
			Dimensions:  qualifiedNames(dims),
		})
	}

	var b strings.Builder
	if len(out.Metrics) == 0 {
		fmt.Fprintf(&b, "No metrics match %q. Call list_metrics with no search argument to see all of them.", in.Search)
	} else {
		fmt.Fprintf(&b, "%d metric(s) in model %q:\n\n", len(out.Metrics), out.Model)
		for _, m := range out.Metrics {
			fmt.Fprintf(&b, "- %s: %s\n", m.Name, m.Description)
			if len(m.Synonyms) > 0 {
				fmt.Fprintf(&b, "    also called: %s\n", strings.Join(m.Synonyms, ", "))
			}
		}
	}
	return textResult(b.String()), out, nil
}

func metricMatches(m *osi.Metric, needle string) bool {
	if strings.Contains(strings.ToLower(m.Name), needle) ||
		strings.Contains(strings.ToLower(m.Description), needle) ||
		strings.Contains(strings.ToLower(m.AIContext.Instructions), needle) {
		return true
	}
	for _, syn := range m.AIContext.Synonyms {
		if strings.Contains(strings.ToLower(syn), needle) {
			return true
		}
	}
	return false
}

// ---------- describe_metric ----------

type describeMetricIn struct {
	Name string `json:"name" jsonschema:"the exact metric name as returned by list_metrics"`
}

type dimensionInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Datatype    string   `json:"datatype,omitempty"`
	IsTime      bool     `json:"is_time"`
	Grains      []string `json:"grains,omitempty"`
	Synonyms    []string `json:"synonyms,omitempty"`
}

type describeMetricOut struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Definition  string          `json:"definition"`
	Aggregation string          `json:"aggregation"`
	Additivity  string          `json:"additivity"`
	Datatype    string          `json:"datatype,omitempty"`
	Synonyms    []string        `json:"synonyms,omitempty"`
	Grain       []string        `json:"grain"`
	Dimensions  []dimensionInfo `json:"dimensions"`
}

func (s *Server) describeMetric(ctx context.Context, _ *mcp.CallToolRequest, in describeMetricIn) (*mcp.CallToolResult, describeMetricOut, error) {
	var out describeMetricOut
	vm, candidates, ok := s.eng.FindMetric(in.Name)
	if !ok {
		if len(candidates) > 0 {
			return errorResult(fmt.Sprintf(
				"%q exists in more than one namespace. Ask for one of: %s.",
				in.Name, strings.Join(candidates, ", "))), out, nil
		}
		return errorResult(fmt.Sprintf(
			"No metric named %q. Call list_metrics to see the available names; do not guess.", in.Name)), out, nil
	}
	m := vm.Metric

	dims, err := s.eng.VisibleDimensions(ctx, s.identity, &vm)
	if err != nil {
		return nil, out, fmt.Errorf("resolving dimension access: %w", err)
	}

	out = describeMetricOut{
		Name:        vm.Qualified,
		Description: m.Description,
		Definition:  m.Expression.ANSI(),
		Aggregation: aggregationOf(m),
		Additivity:  additivityOf(m),
		Datatype:    m.Datatype,
		Synonyms:    m.AIContext.Synonyms,
		Grain:       grainOf(vm),
	}
	for _, d := range dims {
		out.Dimensions = append(out.Dimensions, dimInfo(d))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n\n", vm.Qualified, m.Description)
	fmt.Fprintf(&b, "Definition: %s\n", out.Definition)
	fmt.Fprintf(&b, "Aggregation: %s (%s)\n", out.Aggregation, out.Additivity)
	if len(out.Synonyms) > 0 {
		fmt.Fprintf(&b, "Also called: %s\n", strings.Join(out.Synonyms, ", "))
	}
	b.WriteString("\nDimensions you may group this metric by:\n")
	for _, d := range out.Dimensions {
		fmt.Fprintf(&b, "  - %s", d.Name)
		if d.Description != "" {
			fmt.Fprintf(&b, ": %s", d.Description)
		}
		if d.IsTime {
			b.WriteString(" (time dimension; pass a `grain`)")
		}
		b.WriteString("\n")
	}
	return textResult(b.String()), out, nil
}

// ---------- list_dimensions ----------

type listDimensionsIn struct {
	Metric string `json:"metric,omitempty" jsonschema:"restrict to dimensions valid for this metric"`
}

type listDimensionsOut struct {
	Dimensions []dimensionInfo `json:"dimensions"`
}

func (s *Server) listDimensions(ctx context.Context, _ *mcp.CallToolRequest, in listDimensionsIn) (*mcp.CallToolResult, listDimensionsOut, error) {
	var out listDimensionsOut
	var metric *engine.VisibleMetric
	if in.Metric != "" {
		vm, candidates, ok := s.eng.FindMetric(in.Metric)
		if !ok {
			if len(candidates) > 0 {
				return errorResult(fmt.Sprintf(
					"%q exists in more than one namespace. Ask for one of: %s.",
					in.Metric, strings.Join(candidates, ", "))), out, nil
			}
			return errorResult(fmt.Sprintf(
				"No metric named %q. Call list_metrics to see the available names.", in.Metric)), out, nil
		}
		metric = &vm
	}
	dims, err := s.eng.VisibleDimensions(ctx, s.identity, metric)
	if err != nil {
		return nil, out, fmt.Errorf("resolving dimension access: %w", err)
	}
	for _, d := range dims {
		out.Dimensions = append(out.Dimensions, dimInfo(d))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d dimension(s):\n", len(out.Dimensions))
	for _, d := range out.Dimensions {
		fmt.Fprintf(&b, "  - %s", d.Name)
		if d.Description != "" {
			fmt.Fprintf(&b, ": %s", d.Description)
		}
		b.WriteString("\n")
	}
	return textResult(b.String()), out, nil
}

// ---------- query ----------

type queryIn struct {
	Metrics    []string      `json:"metrics" jsonschema:"metric names from list_metrics; at least one is required"`
	Dimensions []string      `json:"dimensions,omitempty" jsonschema:"dimension names as dataset.field, from list_dimensions"`
	Filters    []plan.Filter `json:"filters,omitempty" jsonschema:"structured filters; each is a dimension, an operator and its values. This is not SQL and no SQL fragment is accepted here"`
	Grain      plan.Grain    `json:"grain,omitempty" jsonschema:"time bucket for the selected time dimension: second, minute, hour, day, week, month, quarter or year"`
	Limit      int           `json:"limit,omitempty" jsonschema:"maximum rows to return"`
	OrderBy    []plan.Order  `json:"order_by,omitempty" jsonschema:"sort by an output column name, which is a metric name or a dimension field name"`
}

type queryOut struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	RowCount     int      `json:"row_count"`
	CompiledSQL  string   `json:"compiled_sql"`
	ModelVersion string   `json:"model_version"`
	Dialect      string   `json:"dialect"`
}

func (s *Server) query(ctx context.Context, _ *mcp.CallToolRequest, in queryIn) (*mcp.CallToolResult, queryOut, error) {
	var out queryOut
	req := plan.Request{
		Metrics: in.Metrics, Dimensions: in.Dimensions, Filters: in.Filters,
		Grain: in.Grain, Limit: in.Limit, OrderBy: in.OrderBy,
	}

	// When there is no executor the engine can still compile. Returning the
	// SQL and saying plainly that nothing ran is more useful than an opaque
	// failure, and it keeps the compile-only deployment demonstrable.
	if !s.eng.CanExecute() {
		c, err := s.eng.Compile(ctx, s.identity, req)
		if err != nil {
			return errorResult(refusalText(err)), out, nil
		}
		out = queryOut{Columns: c.Columns, CompiledSQL: c.SQL,
			ModelVersion: c.ModelVersion, Dialect: c.Dialect}
		return textResult(fmt.Sprintf(
			"This engine is configured to compile but not execute, so no rows were fetched.\n\n"+
				"The request is valid and compiles to:\n\n%s\n", c.SQL)), out, nil
	}

	res, err := s.eng.Query(ctx, s.identity, req)
	if err != nil {
		return errorResult(refusalText(err)), out, nil
	}
	out = queryOut{
		Columns: res.Columns, Rows: res.Rows, RowCount: res.RowCount,
		CompiledSQL: res.CompiledSQL, ModelVersion: res.ModelVersion, Dialect: res.Dialect,
	}
	return textResult(renderTable(res)), out, nil
}

// refusalText renders a refusal for a model to read and act on. The code, the
// reason and the hint all matter: an agent reads the hint and retries, so a
// vague refusal costs a round trip.
func refusalText(err error) string {
	var r *plan.Refusal
	if errors.As(err, &r) {
		var b strings.Builder
		fmt.Fprintf(&b, "Request refused (%s).\n\n%s\n", r.Code, r.Reason)
		if r.Hint != "" {
			fmt.Fprintf(&b, "\nWhat to do: %s\n", r.Hint)
		}
		// Spelled out rather than given as a code, because the reader here is a
		// model and the difference between "change it" and "stop" is what keeps
		// an agent from either giving up on a typo or looping on a denial.
		switch r.Retry() {
		case plan.RetryModify:
			b.WriteString("\nChange the request and try again. Repeating it unchanged will not work.\n")
		case plan.RetryLater:
			b.WriteString("\nNothing about the request is wrong. Try the same request again shortly.\n")
		case plan.RetryNever:
			b.WriteString("\nDo not retry this. No version of this request will succeed for you; " +
				"say so rather than trying a different metric that answers a different question.\n")
		}
		b.WriteString("\nThis is a refusal, not an empty result. Do not report a number for this question.")
		return b.String()
	}
	return "Request failed: " + err.Error()
}

// renderTable formats rows as fixed-width text, which a model reads more
// reliably than nested JSON.
func renderTable(res *engine.Result) string {
	var b strings.Builder
	if res.RowCount == 0 {
		b.WriteString("The query ran and matched no rows. This is a real answer: there is no data for these filters.\n")
	} else {
		widths := make([]int, len(res.Columns))
		cells := make([][]string, 0, len(res.Rows))
		for i, c := range res.Columns {
			widths[i] = len(c)
		}
		for _, row := range res.Rows {
			line := make([]string, len(res.Columns))
			for i := range res.Columns {
				if i < len(row) {
					line[i] = cell(row[i])
				}
				widths[i] = max(widths[i], len(line[i]))
			}
			cells = append(cells, line)
		}
		writeRow(&b, res.Columns, widths)
		seps := make([]string, len(res.Columns))
		for i, w := range widths {
			seps[i] = strings.Repeat("-", w)
		}
		writeRow(&b, seps, widths)
		for _, line := range cells {
			writeRow(&b, line, widths)
		}
		fmt.Fprintf(&b, "\n%d row(s).\n", res.RowCount)
	}
	fmt.Fprintf(&b, "\nModel version: %s\nCompiled SQL (%s):\n%s\n",
		res.ModelVersion, res.Dialect, res.CompiledSQL)
	return b.String()
}

// cell renders one value. A SQL NULL prints as an empty cell rather than as
// Go's <nil>, which a model would otherwise read back as a literal value.
func cell(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func writeRow(b *strings.Builder, cells []string, widths []int) {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = fmt.Sprintf("%-*s", widths[i], c)
	}
	b.WriteString(strings.TrimRight(strings.Join(parts, "  "), " "))
	b.WriteString("\n")
}

// ---------- helpers ----------

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errorResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// aggregationOf names the aggregate functions a metric uses.
func aggregationOf(m *osi.Metric) string {
	aggs := osi.Aggregates(m.Expression.AST)
	if len(aggs) == 0 {
		return "none"
	}
	seen := map[string]bool{}
	var names []string
	for _, a := range aggs {
		name := a.Name
		if a.Distinct {
			name += " DISTINCT"
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// additivityOf explains how the metric behaves when rolled up, derived from the
// spec's decomposability classification rather than from a declared field.
func additivityOf(m *osi.Metric) string {
	aggs := osi.Aggregates(m.Expression.AST)
	if len(aggs) == 0 {
		return "not an aggregate"
	}
	worst := osi.Distributive
	for _, a := range aggs {
		if c := osi.Classify(a); c > worst {
			worst = c
		}
	}
	switch worst {
	case osi.Distributive:
		return "additive: safe to sum across any dimension"
	case osi.Algebraic:
		return "algebraic: recomputed at the requested grain, never summed from finer buckets"
	case osi.Holistic:
		return "non-additive: must be recomputed at the requested grain and cannot be summed"
	case osi.SketchBased:
		return "approximate and non-additive: recomputed at the requested grain"
	}
	return worst.String()
}

// dimInfo renders one visible dimension for a tool response.
func dimInfo(d engine.VisibleDimension) dimensionInfo {
	info := dimensionInfo{
		Name:        d.Qualified,
		Description: d.Field.Description,
		Datatype:    d.Field.Datatype,
		IsTime:      d.Field.IsTime,
		Synonyms:    d.Field.AIContext.Synonyms,
	}
	if d.Field.IsTime {
		info.Grains = grainNames()
	}
	return info
}

// grainOf describes the grain of the datasets a metric reads, which the spec
// expresses as a primary key rather than as prose.
func grainOf(vm engine.VisibleMetric) []string {
	var out []string
	for _, d := range vm.Namespace.Schema.MetricDatasets(vm.Metric) {
		if k := d.Grain(); len(k) > 0 {
			out = append(out, fmt.Sprintf("one row per %s in %s", strings.Join(k, " and "), d.Name))
		} else {
			out = append(out, "undeclared grain in "+d.Name)
		}
	}
	return out
}

func grainNames() []string {
	gs := plan.Grains()
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = string(g)
	}
	return out
}

func qualifiedNames(ds []engine.VisibleDimension) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Qualified
	}
	return out
}
