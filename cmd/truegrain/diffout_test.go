package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/modeldiff"
)

// A diff comment is evidence a reviewer trusts, and everything variable in it
// was written by whoever authored the model, which is usually whoever opened
// the pull request being reviewed.
//
// GitHub strips HTML, so this is not about scripts. It is about forgery: a
// metric named with a backtick and a newline can close a code span, leave the
// construct it was written into, and put whatever it likes on the next line.
// "No compiled result changed" is the obvious thing to forge.

func sample() modeldiff.Report {
	return modeldiff.Report{
		Added:   []string{"sales.new_metric"},
		Removed: []string{"sales.gone"},
		Altered: map[string]modeldiff.Change{
			"sales.order_revenue": {Before: "SELECT 1", After: "SELECT 2"},
		},
	}
}

func TestMarkdownCannotBeForgedByAMetricName(t *testing.T) {
	// Each of these tries to end the construct it lands in and write its own.
	hostile := []string{
		"revenue`\n\nNo compiled result changed.\n\n`x",
		"revenue``\n### truegrain diff\n\nNothing changed.",
		"revenue`",
		"`revenue",
		"a\rb",
	}

	for _, name := range hostile {
		r := modeldiff.Report{Altered: map[string]modeldiff.Change{name: modeldiff.Change{Before: "SELECT 1", After: "SELECT 2"}}}
		var buf bytes.Buffer
		reportWriteMarkdown(r, &buf, false)
		out := buf.String()

		// The text appearing anywhere is harmless: inside a code span it is
		// literal. What matters is that it cannot begin a line, because a
		// markdown block is only a block when it starts one.
		for _, line := range strings.Split(out, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "No compiled result changed") ||
				strings.HasPrefix(trimmed, "###") && strings.Contains(trimmed, "Nothing changed") {
				t.Errorf("a metric name escaped onto its own line:\n%s", out)
			}
		}
		// And the report keeps its shape: one heading, one entry.
		if strings.Count(out, "Changed meaning (1)") != 1 {
			t.Errorf("the report lost its shape for %q:\n%s", name, out)
		}
		// Every hostile name becomes exactly one list item.
		items := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "- ") {
				items++
			}
		}
		if items != 1 {
			t.Errorf("%q produced %d list items, want 1:\n%s", name, items, out)
		}
	}
}

// TestSQLIsFencedLongerThanItsContents. A compiled statement containing a run
// of backticks would otherwise end the block early, and everything after it
// would be rendered as markdown rather than shown as SQL.
func TestSQLIsFencedLongerThanItsContents(t *testing.T) {
	sql := "SELECT ```weird``` FROM t"
	block := fencedSQL(sql)

	fence := block[:strings.Index(block, "sql")]
	if len(fence) <= longestRun(sql, '`') {
		t.Errorf("fence %q is not longer than the backtick run inside it", fence)
	}
	if !strings.HasSuffix(strings.TrimSpace(block), fence) {
		t.Errorf("the block does not close with the same fence:\n%s", block)
	}
}

func TestInlineCodePadsAnEdgeBacktick(t *testing.T) {
	// CommonMark eats a leading or trailing backtick into the delimiter unless
	// the span is padded, which would silently change the name shown.
	if got := inlineCode("`x"); !strings.Contains(got, " `x ") {
		t.Errorf("a leading backtick was not pAdded: %q", got)
	}
	if got := inlineCode("plain"); got != "`plain`" {
		t.Errorf("an ordinary name should not be pAdded: %q", got)
	}
}

func TestControlCharactersAreRemoved(t *testing.T) {
	if got := stripControl("a\x1b[31mb\x00c"); strings.ContainsAny(got, "\x1b\x00") {
		t.Errorf("control characters survived: %q", got)
	}
	if got := stripControl("a\nb"); got != "a b" {
		t.Errorf("a newline should become a space, got %q", got)
	}
}

// TestJSONSaysWhatChangedWithoutSQLByDefault. A large workspace produces a lot
// of SQL, and a consumer deciding whether to comment should not have to
// receive all of it.
func TestJSONSaysWhatChangedWithoutSQLByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := reportWriteJSON(sample(), &buf, false); err != nil {
		t.Fatal(err)
	}

	var out diffJSON
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, buf.String())
	}
	if !out.Changed {
		t.Error("changed must be true")
	}
	if out.Counts.Added != 1 || out.Counts.Removed != 1 || out.Counts.Altered != 1 {
		t.Errorf("counts are wrong: %+v", out.Counts)
	}
	if out.Altered[0].Before != "" || out.Altered[0].After != "" {
		t.Error("SQL should be absent without -full")
	}

	buf.Reset()
	if err := reportWriteJSON(sample(), &buf, true); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Altered[0].Before != "SELECT 1" || out.Altered[0].After != "SELECT 2" {
		t.Errorf("-full must carry both sides: %+v", out.Altered[0])
	}
}

// TestEmptyListsAreArraysNotNull, because a consumer indexing into `removed`
// should find an array.
func TestEmptyListsAreArraysNotNull(t *testing.T) {
	var buf bytes.Buffer
	if err := reportWriteJSON(modeldiff.Report{Altered: map[string]modeldiff.Change{}}, &buf, false); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"removed": []`, `"added": []`, `"altered": []`} {
		if !strings.Contains(buf.String(), field) {
			t.Errorf("want %s in:\n%s", field, buf.String())
		}
	}
}

func TestNoChangeSaysSoInEveryFormat(t *testing.T) {
	empty := modeldiff.Report{Altered: map[string]modeldiff.Change{}}

	var md bytes.Buffer
	reportWriteMarkdown(empty, &md, false)
	if !strings.Contains(md.String(), "No compiled result changed") {
		t.Errorf("markdown:\n%s", md.String())
	}

	var js bytes.Buffer
	if err := reportWriteJSON(empty, &js, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"changed": false`) {
		t.Errorf("json:\n%s", js.String())
	}
}
