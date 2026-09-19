package workspace_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/workspace"
)

func fixtureRoot() string { return filepath.Join("..", "..", "testdata", "workspace") }

func load(t *testing.T, root string, strict bool) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.Load(root, workspace.Options{Strict: strict})
	if err != nil {
		t.Fatalf("loading workspace:\n%v", err)
	}
	return ws
}

func TestTwoTeamsCoexist(t *testing.T) {
	ws := load(t, fixtureRoot(), true)

	if len(ws.Available()) != 2 {
		t.Fatalf("want 2 namespaces, got %d", len(ws.Available()))
	}
	for _, name := range []string{"sales", "marketing"} {
		ns, ok := ws.Namespace(name)
		if !ok || !ns.Available() {
			t.Fatalf("namespace %q did not load", name)
		}
		if len(ns.Owners()) == 0 {
			t.Errorf("namespace %q has no owners", name)
		}
		if ns.Digest == "" {
			t.Errorf("namespace %q has no digest", name)
		}
	}
	if ws.Digest == "" {
		t.Error("workspace has no digest")
	}
	if ws.Legacy {
		t.Error("a workspace with manifests is not legacy mode")
	}
}

// TestImportedDatasetIsGrafted checks marketing can address the dataset sales
// exported, by its bare local name, so its model files stay portable Ossie.
func TestImportedDatasetIsGrafted(t *testing.T) {
	ws := load(t, fixtureRoot(), true)
	mk, _ := ws.Namespace("marketing")

	if _, ok := mk.Schema.Dataset("orders"); !ok {
		t.Fatal("marketing imported sales.orders but cannot address it as `orders`")
	}
	if _, ok := mk.Schema.Field("orders.order_total"); !ok {
		t.Fatal("the grafted dataset lost its fields")
	}
	// The graft must be a copy: sales must not see marketing's local naming.
	sales, _ := ws.Namespace("sales")
	if d, ok := sales.Schema.Dataset("orders"); !ok || d.ImportedFrom != "" {
		if ok && d.ImportedFrom != "" {
			t.Error("grafting into marketing mutated the sales dataset")
		}
	}
}

// TestGovernedNameFollowsOrigin is the security property of the whole design.
//
// A dataset imported into another namespace must stay bound to the grant
// written where it was defined. If the governed name followed the local alias
// instead, `as:` would be an access control bypass: rename the import and the
// policy written against the source stops matching.
func TestGovernedNameFollowsOrigin(t *testing.T) {
	ws := load(t, fixtureRoot(), true)
	mk, _ := ws.Namespace("marketing")

	f, ok := mk.Schema.Field("orders.order_total")
	if !ok {
		t.Fatal("imported field missing")
	}
	if got := f.QualifiedName(); got != "orders.order_total" {
		t.Errorf("marketing addresses it locally as %q", got)
	}
	if got := f.GovernedName(); got != "sales.orders.order_total" {
		t.Fatalf("policy must resolve against the origin; got %q, want %q",
			got, "sales.orders.order_total")
	}
}

// TestGovernedFieldsInExcludesImports checks a namespace-wide policy tag cannot
// reach into another team's columns just because they were imported.
func TestGovernedFieldsInExcludesImports(t *testing.T) {
	ws := load(t, fixtureRoot(), true)
	fields, ok := ws.GovernedFieldsIn("marketing")
	if !ok {
		t.Fatal("marketing not found")
	}
	for _, f := range fields {
		if strings.HasPrefix(f, "sales.") {
			t.Errorf("tagging the marketing namespace reached into %s, which sales owns", f)
		}
	}
}

// ---------- failure modes, each built as a throwaway workspace ----------

// writeWorkspace materialises a workspace from a path-to-content map.
func writeWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// minimalModel is a one-dataset, one-metric Ossie document.
func minimalModel(name, dataset string) string {
	return `version: "0.2.0.dev0"
semantic_model:
  - name: ` + name + `
    description: Test model.
    datasets:
      - name: ` + dataset + `
        source: main.` + dataset + `
        primary_key: [id]
        description: Test dataset.
        fields:
          - name: id
            expression:
              dialects: [{dialect: ANSI_SQL, expression: id}]
            datatype: Integer
            description: Identifier.
          - name: amount
            expression:
              dialects: [{dialect: ANSI_SQL, expression: amount}]
            datatype: Decimal
            description: Amount.
    metrics:
      - name: total_` + dataset + `
        expression:
          dialects: [{dialect: ANSI_SQL, expression: "SUM(` + dataset + `.amount)"}]
        description: Total amount.
`
}

// TestImportOfUnexportedDatasetIsRefused is the default-private guarantee. A
// team must not be able to reach into another's internals by naming them.
func TestImportOfUnexportedDatasetIsRefused(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"a/namespace.yaml": "version: 1\nname: a\nowners: [\"@a\"]\n",
		"a/model.yaml":     minimalModel("a", "widgets"),
		"b/namespace.yaml": "version: 1\nname: b\nowners: [\"@b\"]\nimports:\n  - namespace: a\n    datasets: [widgets]\n",
		"b/model.yaml":     minimalModel("b", "gadgets"),
	})
	ws, err := workspace.Load(root, workspace.Options{Strict: true})
	if err == nil {
		t.Fatal("importing a dataset the owner did not export must be refused")
	}
	if !strings.Contains(err.Error(), "does not export") {
		t.Errorf("the refusal should say the dataset is not exported:\n%v", err)
	}
	// The exporting namespace is unharmed: only the importer is unavailable.
	if a, ok := ws.Namespace("a"); !ok || !a.Available() {
		t.Error("a bad import in b must not break a")
	}
}

func TestImportFromUnknownNamespaceIsRefused(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"b/namespace.yaml": "version: 1\nname: b\nowners: [\"@b\"]\nimports:\n  - namespace: ghost\n    datasets: [widgets]\n",
		"b/model.yaml":     minimalModel("b", "gadgets"),
	})
	if _, err := workspace.Load(root, workspace.Options{Strict: true}); err == nil {
		t.Fatal("importing from a namespace that does not exist must be refused")
	}
}

// TestImportCollisionRequiresAlias covers the case where an import would shadow
// a local dataset. Silently preferring one would be a wrong answer generator.
func TestImportCollisionRequiresAlias(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"a/namespace.yaml": "version: 1\nname: a\nowners: [\"@a\"]\nexports:\n  datasets: [widgets]\n",
		"a/model.yaml":     minimalModel("a", "widgets"),
		"b/namespace.yaml": "version: 1\nname: b\nowners: [\"@b\"]\nimports:\n  - namespace: a\n    datasets: [widgets]\n",
		"b/model.yaml":     minimalModel("b", "widgets"),
	})
	_, err := workspace.Load(root, workspace.Options{Strict: true})
	if err == nil {
		t.Fatal("an import colliding with a local dataset must be refused")
	}
	if !strings.Contains(err.Error(), "alias") {
		t.Errorf("the refusal should point at `as`:\n%v", err)
	}
}

func TestImportAliasResolvesCollision(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"a/namespace.yaml": "version: 1\nname: a\nowners: [\"@a\"]\nexports:\n  datasets: [widgets]\n",
		"a/model.yaml":     minimalModel("a", "widgets"),
		"b/namespace.yaml": "version: 1\nname: b\nowners: [\"@b\"]\nimports:\n  - namespace: a\n    datasets: [{name: widgets, as: a_widgets}]\n",
		"b/model.yaml":     minimalModel("b", "widgets"),
	})
	ws := load(t, root, true)
	b, _ := ws.Namespace("b")
	if _, ok := b.Schema.Dataset("a_widgets"); !ok {
		t.Fatal("the aliased import is not addressable")
	}
	f, ok := b.Schema.Field("a_widgets.amount")
	if !ok {
		t.Fatal("the aliased import lost its fields")
	}
	// Aliasing must not detach the field from the grant written against it.
	if got := f.GovernedName(); got != "a.widgets.amount" {
		t.Errorf("aliasing changed the governed name to %q, which would bypass policy", got)
	}
}

// TestBrokenNamespaceIsIsolated is the availability guarantee. One team must not
// be able to take the layer down for everyone.
func TestBrokenNamespaceIsIsolated(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"good/namespace.yaml": "version: 1\nname: good\nowners: [\"@g\"]\n",
		"good/model.yaml":     minimalModel("good", "widgets"),
		"bad/namespace.yaml":  "version: 1\nname: bad\nowners: [\"@b\"]\n",
		"bad/model.yaml":      "version: \"0.2.0.dev0\"\nsemantic_model:\n  - name: bad\n    datasets: [}}}not yaml\n",
	})

	// Non-strict, as a server runs: the healthy namespace still serves.
	ws, err := workspace.Load(root, workspace.Options{Strict: false})
	if err != nil {
		t.Fatalf("a broken namespace must not fail a non-strict load:\n%v", err)
	}
	if len(ws.Available()) != 1 || ws.Available()[0].Name != "good" {
		t.Fatalf("want only `good` available, got %d", len(ws.Available()))
	}
	failed := ws.Failed()
	if len(failed) != 1 || failed[0].Name != "bad" {
		t.Fatal("the broken namespace should be retained and reported, not dropped")
	}
	if failed[0].Err == nil {
		t.Error("a failed namespace must carry why it failed")
	}

	// Strict, as CI runs: the same workspace fails the gate.
	if _, err := workspace.Load(root, workspace.Options{Strict: true}); err == nil {
		t.Fatal("a broken namespace must fail a strict load")
	}
}

// TestMalformedManifestIsNeverIsolated: a broken manifest leaves the shape of
// the workspace undefined, so it fails the load outright rather than taking one
// namespace offline.
func TestMalformedManifestIsNeverIsolated(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"good/namespace.yaml": "version: 1\nname: good\nowners: [\"@g\"]\n",
		"good/model.yaml":     minimalModel("good", "widgets"),
		"bad/namespace.yaml":  "version: 1\nname: bad\n",
		"bad/model.yaml":      minimalModel("bad", "gadgets"),
	})
	_, err := workspace.Load(root, workspace.Options{Strict: false})
	if err == nil {
		t.Fatal("a manifest with no owners must fail the whole load")
	}
	if !strings.Contains(err.Error(), "owners") {
		t.Errorf("the error should name the missing field:\n%v", err)
	}
}

func TestDuplicateNamespaceNameIsRefused(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"one/namespace.yaml": "version: 1\nname: dup\nowners: [\"@a\"]\n",
		"one/model.yaml":     minimalModel("dup", "widgets"),
		"two/namespace.yaml": "version: 1\nname: dup\nowners: [\"@b\"]\n",
		"two/model.yaml":     minimalModel("dup", "gadgets"),
	})
	if _, err := workspace.Load(root, workspace.Options{Strict: false}); err == nil {
		t.Fatal("two directories claiming the same namespace name must be refused")
	}
}

// TestLegacySingleModelStillWorks keeps a single-team repository working with
// no manifest at all, so adopting namespaces is an addition and not a
// migration.
func TestLegacySingleModelStillWorks(t *testing.T) {
	ws := load(t, filepath.Join("..", "..", "testdata", "models", "retail.yaml"), true)
	if !ws.Legacy {
		t.Error("a path with no manifests should load in legacy mode")
	}
	ns, ok := ws.Single()
	if !ok {
		t.Fatal("legacy mode should produce exactly one namespace")
	}
	if ns.Name != "retail" {
		t.Errorf("the implicit namespace takes the model name, got %q", ns.Name)
	}
}

// TestDigestIsStable pins that the same bytes produce the same digest, which is
// what makes the workspace version usable as evidence across a fleet.
func TestDigestIsStable(t *testing.T) {
	a := load(t, fixtureRoot(), true)
	b := load(t, fixtureRoot(), true)
	if a.Digest != b.Digest {
		t.Fatalf("two loads of the same tree disagree: %s and %s", a.Digest, b.Digest)
	}
	for _, ns := range a.Namespaces() {
		other, _ := b.Namespace(ns.Name)
		if ns.Digest != other.Digest {
			t.Errorf("namespace %q digest is unstable", ns.Name)
		}
	}
}

func TestFindMetricQualifiedAndBare(t *testing.T) {
	ws := load(t, fixtureRoot(), true)

	if ref, _, ok := ws.FindMetric("marketing.campaign_spend"); !ok {
		t.Error("a qualified name must resolve")
	} else if ref.Qualified != "marketing.campaign_spend" {
		t.Errorf("got %q", ref.Qualified)
	}

	// Unambiguous bare names resolve, which is what keeps single-team usage
	// and every existing command working.
	if ref, _, ok := ws.FindMetric("campaign_spend"); !ok {
		t.Error("an unambiguous bare name must resolve")
	} else if ref.Namespace.Name != "marketing" {
		t.Errorf("resolved into the wrong namespace: %q", ref.Namespace.Name)
	}

	if _, _, ok := ws.FindMetric("nope"); ok {
		t.Error("an unknown metric must not resolve")
	}
}

// TestAmbiguousBareNameNamesCandidates: when the same metric name exists in two
// namespaces the engine refuses and says which to pick, rather than guessing.
func TestAmbiguousBareNameNamesCandidates(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"a/namespace.yaml": "version: 1\nname: a\nowners: [\"@a\"]\n",
		"a/model.yaml":     minimalModel("a", "widgets"),
		"b/namespace.yaml": "version: 1\nname: b\nowners: [\"@b\"]\n",
		"b/model.yaml":     minimalModel("b", "widgets"),
	})
	ws := load(t, root, true)

	_, candidates, ok := ws.FindMetric("total_widgets")
	if ok {
		t.Fatal("a name defined in two namespaces must not resolve silently")
	}
	if len(candidates) != 2 {
		t.Fatalf("want 2 candidates, got %v", candidates)
	}
	for _, want := range []string{"a.total_widgets", "b.total_widgets"} {
		found := false
		for _, c := range candidates {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("candidates should include %q, got %v", want, candidates)
		}
	}
}
