package osi

import (
	"fmt"
	"sort"
	"strings"
)

// Diag is a single model problem, located in the source. Every parser and
// resolver failure produces one of these so that `truegrain validate` can report
// all of a model's problems in one pass instead of one per run.
type Diag struct {
	Pos Pos
	// Subject names the semantic object at fault, for example
	// "metric net_revenue". It is empty for file-level problems.
	Subject string
	Msg     string
	// Hint is an optional actionable next step. Interfaces surface it to the
	// caller, and for an agent it is the difference between a retry that
	// works and a retry that repeats the same mistake.
	Hint string
}

func (d Diag) Error() string {
	var b strings.Builder
	if d.Pos.Line > 0 || d.Pos.File != "" {
		b.WriteString(d.Pos.String())
		b.WriteString(": ")
	}
	if d.Subject != "" {
		b.WriteString(d.Subject)
		b.WriteString(": ")
	}
	b.WriteString(d.Msg)
	if d.Hint != "" {
		b.WriteString("\n  hint: ")
		b.WriteString(d.Hint)
	}
	return b.String()
}

// Diags is an accumulating error list.
type Diags []Diag

func (ds Diags) Error() string {
	switch len(ds) {
	case 0:
		return "no errors"
	case 1:
		return ds[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d model errors:\n", len(ds))
	for i, d := range ds {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, strings.ReplaceAll(d.Error(), "\n  ", "\n     "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ErrOrNil returns nil for an empty list so callers can `return ds.ErrOrNil()`.
func (ds Diags) ErrOrNil() error {
	if len(ds) == 0 {
		return nil
	}
	return ds
}

// Sorted orders diagnostics by file then line, so output is stable across runs
// and diffable in CI.
func (ds Diags) Sorted() Diags {
	out := append(Diags(nil), ds...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pos.File != out[j].Pos.File {
			return out[i].Pos.File < out[j].Pos.File
		}
		return out[i].Pos.Line < out[j].Pos.Line
	})
	return out
}

// collector accumulates diagnostics during a parse or resolve pass.
type collector struct {
	diags Diags
	file  string
}

func (c *collector) add(pos Pos, subject, msg string) {
	if pos.File == "" {
		pos.File = c.file
	}
	c.diags = append(c.diags, Diag{Pos: pos, Subject: subject, Msg: msg})
}

func (c *collector) addHint(pos Pos, subject, msg, hint string) {
	if pos.File == "" {
		pos.File = c.file
	}
	c.diags = append(c.diags, Diag{Pos: pos, Subject: subject, Msg: msg, Hint: hint})
}
