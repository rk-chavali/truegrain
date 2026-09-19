package resolve_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/resolve"
)

// TestUpstreamExamples loads the example models published in the Apache Ossie
// repository. They are vendored under testdata/ossie-examples and are the only
// models in existence that nobody here wrote, which makes them the honest test
// of whether this engine reads the spec or reads its own assumptions.
//
// The test does not require them to be clean. It requires the engine to have an
// opinion about them that a human can read, and it fails if the parser cannot
// get through the file at all.
func TestUpstreamExamples(t *testing.T) {
	for _, name := range []string{"tpcds_semantic_model.yaml"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "testdata", "ossie-examples", name)
			m, err := osi.Load(path)
			if err != nil {
				t.Fatalf("parse failed, the engine cannot read a published Ossie example:\n%v", err)
			}
			t.Logf("model %q, spec version %s, model version %s", m.Name, m.SpecVersion, m.Version)
			t.Logf("%d datasets, %d relationships, %d metrics",
				len(m.Datasets), len(m.Relationships), len(m.Metrics))

			s, err := resolve.New(m)
			if err != nil {
				t.Logf("resolve reported:\n%v", err)
				return
			}
			t.Logf("resolved clean, %d dimensions offered", len(s.Dimensions()))
		})
	}
}

// TestOntologyDocumentRefused pins the behaviour for the other document kind
// Apache Ossie defines. flights.yaml is an ontology, not a semantic model, and
// the engine must say which rather than complaining about a missing key.
func TestOntologyDocumentRefused(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "ossie-examples", "flights.yaml")
	_, err := osi.Load(path)
	if err == nil {
		t.Fatal("expected an ontology document to be refused")
	}
	if !strings.Contains(err.Error(), "ontology document") {
		t.Fatalf("refusal does not name the document kind, so an author cannot act on it:\n%v", err)
	}
}
