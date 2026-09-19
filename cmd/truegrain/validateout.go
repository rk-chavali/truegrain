package main

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/rk-chavali/truegrain/internal/workspace"
)

// The machine-readable form of `truegrain validate`.
//
// Separate from the text form rather than derived from it, because the two
// answer different questions. A person reads the text output to understand a
// workspace; a pipeline reads this to decide whether to continue, and then to
// say something specific about why it stopped.

// validationJSON is the published shape. Adding a field is safe; changing what
// one means is not.
type validationJSON struct {
	// OK is the field a check reads. Everything else explains it.
	OK bool `json:"ok"`
	// Path is the workspace or file this result is about.
	Path string `json:"path"`
	// Kind distinguishes a multi-namespace workspace from a single legacy model.
	Kind string `json:"kind,omitempty"`
	// Config names where settings came from, which is the first thing to check
	// when a pipeline and a laptop disagree.
	Config string `json:"config,omitempty"`
	// Digest is the workspace revision. Two runs reporting the same digest
	// compiled the same model, whatever the branch was called.
	Digest     string          `json:"digest,omitempty"`
	Namespaces []namespaceJSON `json:"namespaces,omitempty"`
	Policy     *policyJSON     `json:"policy,omitempty"`
	// Errors is populated only when OK is false. Each entry is one problem,
	// already carrying its file and line, so a pipeline can annotate.
	Errors []string `json:"errors,omitempty"`
}

type namespaceJSON struct {
	Name          string   `json:"name"`
	Owners        []string `json:"owners,omitempty"`
	Digest        string   `json:"digest"`
	SpecVersion   string   `json:"ossie_spec_version,omitempty"`
	Datasets      int      `json:"datasets"`
	Relationships int      `json:"relationships"`
	Metrics       int      `json:"metrics"`
	Dimensions    int      `json:"dimensions"`
}

type policyJSON struct {
	Path            string `json:"path"`
	ProtectedFields int    `json:"protected_fields"`
}

func writeValidation(out io.Writer, path, config, policyPath string, protected int, ws *workspace.Workspace) error {
	kind := "workspace"
	if ws.Legacy {
		kind = "model"
	}

	result := validationJSON{
		OK:         true,
		Path:       path,
		Kind:       kind,
		Config:     config,
		Digest:     ws.Digest,
		Namespaces: []namespaceJSON{},
	}
	for _, ns := range ws.Available() {
		entry := namespaceJSON{
			Name:          ns.Name,
			Digest:        ns.Digest,
			SpecVersion:   ns.Model.SpecVersion,
			Datasets:      len(ns.Model.Datasets),
			Relationships: len(ns.Model.Relationships),
			Metrics:       len(ns.Model.Metrics),
			Dimensions:    len(ns.Schema.Dimensions()),
		}
		if owners := ns.Owners(); len(owners) > 0 {
			entry.Owners = owners
		}
		result.Namespaces = append(result.Namespaces, entry)
	}
	if policyPath != "" {
		result.Policy = &policyJSON{Path: policyPath, ProtectedFields: protected}
	}

	return encode(out, result)
}

// writeValidationFailure reports a workspace that would not load.
//
// The error text is split back into lines because the loader reports every
// problem it found rather than the first, and a pipeline annotating a pull
// request wants them one at a time.
func writeValidationFailure(out io.Writer, path string, err error) {
	_ = encode(out, validationJSON{
		OK:     false,
		Path:   path,
		Errors: splitProblems(err.Error()),
	})
}

func splitProblems(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func encode(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
