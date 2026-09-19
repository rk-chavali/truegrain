package osi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SupportedSpecVersions are the Ossie core spec versions this engine has been
// tested against. A model declaring anything else still loads, but validate
// reports it, because the spec is a moving draft and a silent version skew is
// how a compiled number quietly changes meaning.
var SupportedSpecVersions = []string{"0.2.0.dev0", "0.2.0"}

// file is the on-disk shape: a top-level spec version plus a list of models.
//
// Ontology is detected but not read. Apache Ossie covers two documents, a
// semantic model and an ontology, and this engine implements only the first.
// Recognising the second lets the error name what the file actually is.
type file struct {
	Version       string    `yaml:"version"`
	SemanticModel []*Model  `yaml:"semantic_model"`
	Ontology      yaml.Node `yaml:"ontology"`
}

// Load reads a model from a file or a directory of files. A directory is read
// non-recursively for .yaml and .yml, in sorted order, and models sharing a
// name are merged so a large model can be split across files.
// notAModel names the sidecars that live beside a model directory and are
// not models.
//
// Loading a directory means loading every YAML in it, which is the right
// default: an author splitting one model across five files should not have
// to list them. It is wrong for the files this engine puts next to a model
// for its own purposes, and the failure is loud and confusing rather than
// quiet: `truegrain.yaml` parses as YAML, has no semantic_model list, and
// the model load fails with "no semantic_model entries" pointing at a file
// nobody thought was a model.
//
// Spelled out here rather than imported, because internal/config and
// internal/rollup both depend on this package and cannot be depended on
// back. TestADirectoryLoadSkipsTheSidecars keeps the two lists in step.
var notAModel = map[string]bool{
	"truegrain.yaml": true,
	"rollups.yaml":   true,
	"namespace.yaml": true,
}

func Load(path string) (*Model, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading model path: %w", err)
	}
	if !info.IsDir() {
		return LoadFiles([]string{path})
	}
	var paths []string
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("reading model directory: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if notAModel[strings.ToLower(e.Name())] {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(e.Name())); ext == ".yaml" || ext == ".yml" {
			paths = append(paths, filepath.Join(path, e.Name()))
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no .yaml or .yml files in %s", path)
	}
	sort.Strings(paths)
	return LoadFiles(paths)
}

// LoadFiles parses the given files into a single model.
func LoadFiles(paths []string) (*Model, error) {
	var (
		c        collector
		merged   *Model
		specVers = map[string]bool{}
		hasher   = sha256.New()
	)

	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		// Hash filename and content so the model version changes when either
		// the definitions or their layout change.
		fmt.Fprintf(hasher, "%s\x00%d\x00", filepath.Base(p), len(src))
		hasher.Write(src)

		c.file = p
		var f file
		if err := yaml.Unmarshal(src, &f); err != nil {
			// A YAML syntax error already carries a line; surface it as-is.
			c.add(Pos{File: p}, "", fmt.Sprintf("invalid YAML: %v", err))
			continue
		}
		if f.Version != "" {
			specVers[f.Version] = true
		}
		if len(f.SemanticModel) == 0 {
			if f.Ontology.Kind != 0 {
				c.addHint(Pos{File: p, Line: f.Ontology.Line}, "",
					"this is an Ossie ontology document, not a semantic model",
					"the engine compiles semantic models (`semantic_model:` with datasets, relationships and metrics); ontology documents are not supported, see internal/osi/COMPLIANCE.md")
				continue
			}
			c.addHint(Pos{File: p}, "", "no semantic_model entries",
				"the file must have a top-level `semantic_model:` list")
			continue
		}
		for _, m := range f.SemanticModel {
			m.Pos.File = p
			stampFile(m, p)
			if merged == nil {
				merged = m
				continue
			}
			if m.Name != merged.Name {
				c.addHint(m.Pos, "semantic_model "+m.Name,
					fmt.Sprintf("a second model name in the same load (%q and %q)", merged.Name, m.Name),
					"v1 serves one model per engine; point --models at a directory containing only one")
				continue
			}
			merged.Datasets = append(merged.Datasets, m.Datasets...)
			merged.Relationships = append(merged.Relationships, m.Relationships...)
			merged.Metrics = append(merged.Metrics, m.Metrics...)
		}
	}

	if merged == nil {
		if len(c.diags) == 0 {
			c.add(Pos{}, "", "no semantic model found")
		}
		return nil, c.diags.Sorted()
	}

	merged.Version = "sha256:" + hex.EncodeToString(hasher.Sum(nil))[:12]
	merged.SpecVersion = joinVersions(specVers)
	checkSpecVersion(&c, merged)

	parseExpressions(&c, merged)

	if err := c.diags.Sorted().ErrOrNil(); err != nil {
		return nil, err
	}
	return merged, nil
}

func joinVersions(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func checkSpecVersion(c *collector, m *Model) {
	if m.SpecVersion == "" {
		c.addHint(m.Pos, "", "no top-level `version:` declaring the Ossie spec version",
			"add `version: \"0.2.0.dev0\"` at the top of the file")
		return
	}
	if slices.Contains(SupportedSpecVersions, m.SpecVersion) {
		return
	}
	c.addHint(m.Pos, "", fmt.Sprintf("spec version %q is not one this engine has been tested against (%s)",
		m.SpecVersion, strings.Join(SupportedSpecVersions, ", ")),
		"the Ossie core spec is a published draft; check internal/osi/COMPLIANCE.md before relying on this model")
}

// stampFile propagates the source path into every nested position, which the
// YAML decoder cannot do because it only sees line numbers.
func stampFile(m *Model, path string) {
	m.Pos.File = path
	for _, d := range m.Datasets {
		d.Pos.File = path
		for _, f := range d.Fields {
			f.Pos.File = path
			f.Expression.Pos.File = path
			f.Dataset = d
		}
	}
	for _, r := range m.Relationships {
		r.Pos.File = path
	}
	for _, x := range m.Metrics {
		x.Pos.File = path
		x.Expression.Pos.File = path
	}
}

// parseExpressions parses the ANSI_SQL variant of every field and metric. This
// runs at load time rather than at query time so a broken expression is a
// validate-time failure, not a runtime surprise for whoever asks the question.
func parseExpressions(c *collector, m *Model) {
	for _, d := range m.Datasets {
		for _, f := range d.Fields {
			parseOne(c, &f.Expression, fmt.Sprintf("field %s.%s", d.Name, f.Name))
		}
	}
	for _, x := range m.Metrics {
		parseOne(c, &x.Expression, "metric "+x.Name)
	}
}

func parseOne(c *collector, e *Expression, subject string) {
	if len(e.ByDialect) == 0 {
		c.addHint(e.Pos, subject, "no expression",
			"every field and metric requires an `expression.dialects` block")
		return
	}
	src, ok := e.ByDialect[DialectANSI]
	if !ok {
		// The spec permits dialect-only expressions, but the engine plans over
		// ANSI. Without it there is nothing to analyse, so refuse rather than
		// pick an arbitrary dialect and plan against the wrong semantics.
		have := make([]string, 0, len(e.ByDialect))
		for d := range e.ByDialect {
			have = append(have, d)
		}
		sort.Strings(have)
		c.addHint(e.Pos, subject,
			fmt.Sprintf("no %s expression (found: %s)", DialectANSI, strings.Join(have, ", ")),
			"the planner analyses the ANSI_SQL variant; add one alongside the dialect-specific expressions")
		return
	}
	ast, err := ParseExpr(src)
	if err != nil {
		c.add(e.Pos, subject, fmt.Sprintf("cannot parse expression: %v", err))
		return
	}
	e.AST = ast
}
