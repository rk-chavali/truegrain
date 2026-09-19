package govern

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rk-chavali/truegrain/internal/plan"
)

// Row-level access: which rows a caller may see, as opposed to which columns.
//
// Column policy answers "may this caller read the email address". This answers
// "may this caller read the rows belonging to another region". Until now only
// the first question was asked, which meant a regional manager who could see
// revenue could see everyone's revenue.
//
// The mechanism is deliberately boring: a policy contributes filters to the
// request before it is planned, and the planner treats them exactly like the
// caller's own. No new SQL path, no second emitter, nothing that could produce
// a statement the gate has not seen.
//
// Four properties are load bearing, and each has a test:
//
//   - Policy filters are added, never substituted. They combine with the
//     caller's own by AND, so narrowing a query cannot widen it.
//   - A caller cannot remove one. They are injected after the request is
//     parsed and there is no request field that names them.
//   - A filter that will not resolve refuses the query. Silently dropping one
//     is a data leak wearing the costume of a permissive default.
//   - They restrict the principals they name and nobody else. An unfiltered
//     namespace shows every row, which matches how an untagged column is
//     readable, and health says how many principals are covered so a partial
//     policy is visible rather than assumed complete.

// RowPolicy restricts which rows one principal may see in one namespace.
type RowPolicy struct {
	// Namespace the policy applies to. The filter is added to any query whose
	// metrics come from it.
	Namespace string
	// Principal is a subject or a group, matched the same way a column grant is.
	Principal string
	// Filter is added to the request. Its dimension is resolved inside the
	// namespace, so it is written the way a caller would write it.
	Filter plan.Filter
	// Description is for the operator reading health, not for the caller.
	Description string
}

// RowPolicies is the set loaded from a policy file.
type RowPolicies []RowPolicy

// For returns the filters that apply to this identity in this namespace.
//
// Order is stable so two compiles of the same request produce the same SQL,
// which the diff command depends on.
func (r RowPolicies) For(id Identity, namespace string) []plan.Filter {
	if len(r) == 0 {
		return nil
	}
	principals := id.principals()

	var out []plan.Filter
	for _, p := range r {
		if !strings.EqualFold(p.Namespace, namespace) {
			continue
		}
		if slices.Contains(principals, p.Principal) {
			out = append(out, p.Filter)
		}
	}
	return out
}

// Principals counts who is covered, for health. It never names them: who is
// restricted is itself worth not publishing to a caller.
func (r RowPolicies) Principals() int {
	seen := map[string]bool{}
	for _, p := range r {
		seen[p.Principal] = true
	}
	return len(seen)
}

// Validate checks the policies against the workspace before anything serves
// them.
//
// Strict for the same reason the column policy is strict: a namespace name
// that does not exist is almost always a rename, and the failure mode is a
// restriction everybody believes is in force quietly applying to nothing.
func (r RowPolicies) Validate(namespaces []string) error {
	var problems []string
	for _, p := range r {
		switch {
		case p.Namespace == "":
			problems = append(problems, "a row policy names no namespace")
		case !containsFold(namespaces, p.Namespace):
			problems = append(problems, fmt.Sprintf(
				"row policy for %q names namespace %q, which this workspace does not have",
				p.Principal, p.Namespace))
		}
		if p.Principal == "" {
			problems = append(problems, fmt.Sprintf(
				"a row policy on %q names no principal, so it would restrict nobody", p.Namespace))
		}
		if p.Filter.Dimension == "" {
			problems = append(problems, fmt.Sprintf(
				"the row policy for %q on %q has no dimension", p.Principal, p.Namespace))
		}
		if p.Filter.Op == "" {
			problems = append(problems, fmt.Sprintf(
				"the row policy for %q on %q has no operator", p.Principal, p.Namespace))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("row policy has errors:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}
