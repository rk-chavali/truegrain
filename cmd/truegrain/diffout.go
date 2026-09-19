package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/modeldiff"
)

// Machine-readable and review-readable renderings of a diff.
//
// The text form is for a person at a terminal. These two are for a pipeline:
// JSON so a check can decide something, and markdown so a pull request comment
// needs no renderer on the other side. The binary already knows the answer, and
// asking a workflow to reformat JSON into markdown means either a scripting
// dependency in the action or a second implementation that drifts.
//
// Both are built from the same report, so the three can never disagree about
// what changed.

// diffJSON is the published shape. It is a contract: a field may be added, and
// an existing one may not change meaning without a major version.
type diffJSON struct {
	// Changed is the single field most consumers read.
	Changed bool `json:"changed"`
	// Counts saves a consumer from measuring three arrays to decide whether to
	// comment at all.
	Counts diffCounts `json:"counts"`
	// Removed names requests that used to compile and now do not. Listed first
	// because they break a caller outright.
	Removed []string `json:"removed"`
	// Altered names requests that still compile and now mean something else.
	// This is the dangerous category: nothing fails, and every number moves.
	Altered []alteredJSON `json:"altered"`
	// Added names requests that are newly answerable.
	Added []string `json:"added"`
}

type diffCounts struct {
	Removed int `json:"removed"`
	Altered int `json:"altered"`
	Added   int `json:"added"`
}

type alteredJSON struct {
	Label string `json:"label"`
	// Before and After carry the compiled SQL, and are present only with -full.
	// A large workspace produces a lot of SQL, and a consumer that only needs
	// to know something changed should not have to receive all of it.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

func reportToJSON(r modeldiff.Report, full bool) diffJSON {
	out := diffJSON{
		Changed: r.Changed(),
		Counts: diffCounts{
			Removed: len(r.Removed),
			Altered: len(r.Altered),
			Added:   len(r.Added),
		},
		// Empty slices rather than nil: a consumer indexing into `removed`
		// should find an array, not null.
		Removed: append([]string{}, r.Removed...),
		Added:   append([]string{}, r.Added...),
		Altered: []alteredJSON{},
	}

	labels := make([]string, 0, len(r.Altered))
	for label := range r.Altered {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	for _, label := range labels {
		entry := alteredJSON{Label: label}
		if full {
			pair := r.Altered[label]
			entry.Before, entry.After = pair.Before, pair.After
		}
		out.Altered = append(out.Altered, entry)
	}
	return out
}

func reportWriteJSON(r modeldiff.Report, out io.Writer, full bool) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(reportToJSON(r, full))
}

// writeMarkdown renders a pull request comment.
//
// Everything variable here is written by whoever authored the model, which is
// frequently whoever opened the pull request being reviewed. A comment is
// evidence a reviewer trusts, so a metric name must not be able to forge it:
// GitHub strips HTML, but a name containing backticks and newlines can close a
// code fence early and write whatever it likes after it, including a line
// saying nothing changed. Every label and every block below is fenced with a
// run longer than anything inside it, and control characters are removed.
func reportWriteMarkdown(r modeldiff.Report, out io.Writer, full bool) {
	if !r.Changed() {
		fmt.Fprintln(out, "### truegrain diff")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "No compiled result changed. Every metric still means what it meant.")
		return
	}

	fmt.Fprintln(out, "### truegrain diff")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "**%s.** This pull request changes what the model computes.\n\n",
		summarise(r))

	if len(r.Removed) > 0 {
		fmt.Fprintf(out, "#### Removed (%d)\n\n", len(r.Removed))
		fmt.Fprint(out, "Anything asking for these now fails.\n\n")
		for _, label := range r.Removed {
			fmt.Fprintf(out, "- %s\n", inlineCode(label))
		}
		fmt.Fprintln(out)
	}

	if len(r.Altered) > 0 {
		labels := make([]string, 0, len(r.Altered))
		for label := range r.Altered {
			labels = append(labels, label)
		}
		sort.Strings(labels)

		fmt.Fprintf(out, "#### Changed meaning (%d)\n\n", len(labels))
		fmt.Fprint(out, "These still compile. They compute something different.\n\n")
		for _, label := range labels {
			if !full {
				fmt.Fprintf(out, "- %s\n", inlineCode(label))
				continue
			}
			pair := r.Altered[label]
			fmt.Fprintf(out, "<details><summary>%s</summary>\n\n", inlineCode(label))
			fmt.Fprintln(out, "Before:")
			fmt.Fprintln(out, fencedSQL(pair.Before))
			fmt.Fprintln(out, "After:")
			fmt.Fprintln(out, fencedSQL(pair.After))
			fmt.Fprintln(out, "</details>")
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out)
	}

	if len(r.Added) > 0 {
		fmt.Fprintf(out, "#### Added (%d)\n\n", len(r.Added))
		for _, label := range r.Added {
			fmt.Fprintf(out, "- %s\n", inlineCode(label))
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintln(out, "A change to compiled output is a change to every number downstream,")
	fmt.Fprintln(out, "even when nothing about the API moved. If this is intended, say so here.")
}

// summarise is the one line someone reads without expanding anything.
func summarise(r modeldiff.Report) string {
	parts := []string{}
	if n := len(r.Removed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", n))
	}
	if n := len(r.Altered); n > 0 {
		parts = append(parts, fmt.Sprintf("%d changed meaning", n))
	}
	if n := len(r.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d added", n))
	}
	return strings.Join(parts, ", ")
}

// inlineCode wraps s in a backtick run longer than any run inside it.
//
// This is the CommonMark rule for inline code, and following it is what stops
// a crafted name from ending the span and being read as markdown.
func inlineCode(s string) string {
	clean := stripControl(s)
	fence := strings.Repeat("`", longestRun(clean, '`')+1)
	// A leading or trailing backtick inside the span needs padding spaces, or
	// the parser eats it as part of the delimiter.
	if strings.HasPrefix(clean, "`") || strings.HasSuffix(clean, "`") {
		return fence + " " + clean + " " + fence
	}
	return fence + clean + fence
}

// fencedSQL wraps a block in a fence longer than any backtick run inside it.
func fencedSQL(sql string) string {
	clean := strings.TrimSpace(stripControl(sql))
	fence := strings.Repeat("`", max(3, longestRun(clean, '`')+1))
	return fence + "sql\n" + clean + "\n" + fence
}

// longestRun returns the length of the longest consecutive run of c.
func longestRun(s string, c byte) int {
	longest, current := 0, 0
	for i := range len(s) {
		if s[i] == c {
			current++
			if current > longest {
				longest = current
			}
			continue
		}
		current = 0
	}
	return longest
}

// stripControl removes characters that would let a value escape the construct
// it is written into. Newlines and carriage returns end a markdown line, which
// is the whole trick; the rest are removed because a terminal control sequence
// in a review comment has no legitimate reason to be there.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
