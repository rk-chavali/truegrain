package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
	"github.com/rk-chavali/truegrain/internal/resolve"
	"github.com/rk-chavali/truegrain/internal/rollup"
)

// skipDirs are never walked when discovering namespaces.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"bin": true, "build": true, "dist": true, ".venv": true,
}

// Namespace is one independently owned unit of the workspace.
//
// A namespace is a complete planning universe: its own datasets, its own
// relationships, its own metrics, plus any datasets it imported, already
// aliased to their local names. That is what lets the planner stay entirely
// unaware that composition exists.
type Namespace struct {
	Name     string
	Manifest *Manifest
	Model    *osi.Model
	Schema   *resolve.Schema
	// Rollups are the pre-aggregated tables this namespace declares, read
	// from its rollups.yaml and validated against Schema. Nil where there
	// is no sidecar, which is every namespace that predates them.
	Rollups []*rollup.Rollup
	// Digest is the content hash of this namespace's model files.
	Digest string
	// Err is non-nil when the namespace failed to load. A failed namespace is
	// retained rather than dropped so `health` can say which one broke and why.
	Err error
}

// Available reports whether the namespace loaded and can serve queries.
func (n *Namespace) Available() bool { return n.Err == nil && n.Schema != nil }

// Owners is the namespace's declared owners, or nil in legacy single-model mode.
func (n *Namespace) Owners() []string {
	if n.Manifest == nil {
		return nil
	}
	return n.Manifest.Owners
}

// Workspace is a set of namespaces loaded from one root.
type Workspace struct {
	// Root is the directory or file the workspace was loaded from.
	Root string
	// Digest is the hash over every namespace digest, and is the version that
	// appears in query responses and audit events.
	Digest string
	// Legacy reports that no namespace manifests were found and the root was
	// loaded as a single unnamespaced model.
	Legacy bool

	byName map[string]*Namespace
	order  []string
}

// Options control how a workspace is loaded.
type Options struct {
	// Discover overrides the default walk with explicit globs matching
	// namespace directories.
	Discover []string
	// Strict fails the whole load when any namespace fails. Commands where a
	// human is watching set this; a running server does not, so that one team's
	// broken file cannot take the layer down for everyone.
	Strict bool
}

// Load reads a workspace from a directory.
//
// When no namespace manifest is found anywhere under root, the path is loaded
// as a single Ossie model and the workspace holds one namespace named after
// that model. This keeps a single-team repository working with no manifest at
// all, and means adopting namespaces is an addition rather than a migration.
func Load(root string, opts Options) (*Workspace, error) {
	manifests, err := discover(root, opts.Discover)
	if err != nil {
		return nil, err
	}
	if len(manifests) == 0 {
		return loadLegacy(root)
	}
	return loadNamespaced(root, manifests, opts)
}

// discover finds namespace manifests, either by walking or by explicit globs.
func discover(root string, globs []string) ([]*Manifest, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("reading workspace path: %w", err)
	}
	if !info.IsDir() {
		return nil, nil // a single file is always legacy mode
	}

	var paths []string
	if len(globs) > 0 {
		for _, g := range globs {
			matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(g)))
			if err != nil {
				return nil, fmt.Errorf("invalid discover pattern %q: %w", g, err)
			}
			for _, dir := range matches {
				candidate := filepath.Join(dir, ManifestFile)
				if _, err := os.Stat(candidate); err == nil {
					paths = append(paths, candidate)
				}
			}
		}
	} else {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() == ManifestFile {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("discovering namespaces: %w", err)
		}
	}

	sort.Strings(paths)
	var out []*Manifest
	var problems []string
	for _, p := range paths {
		m, err := LoadManifest(p)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		out = append(out, m)
	}
	if len(problems) > 0 {
		// A malformed manifest is never isolated. It is the file that declares
		// ownership and visibility, so a broken one leaves the workspace shape
		// undefined rather than merely leaving one namespace unavailable.
		return nil, fmt.Errorf("invalid namespace manifest(s):\n  %s", strings.Join(problems, "\n  "))
	}
	return out, nil
}

// loadLegacy loads a path with no manifests as one implicit namespace.
func loadLegacy(root string) (*Workspace, error) {
	model, err := osi.Load(root)
	if err != nil {
		return nil, err
	}
	for _, d := range model.Datasets {
		d.Namespace = model.Name
	}
	schema, err := resolve.New(model)
	if err != nil {
		return nil, err
	}
	// The sidecar sits beside the model, whether the model is a directory
	// or a single file.
	dir := root
	if info, statErr := os.Stat(root); statErr == nil && !info.IsDir() {
		dir = filepath.Dir(root)
	}
	rollups, err := rollup.Load(filepath.Join(dir, rollup.FileName), schema)
	if err != nil {
		return nil, err
	}
	ns := &Namespace{
		Name:    model.Name,
		Model:   model,
		Schema:  schema,
		Rollups: rollups,
		Digest:  model.Version,
	}
	return &Workspace{
		Root:   root,
		Digest: model.Version,
		Legacy: true,
		byName: map[string]*Namespace{norm(model.Name): ns},
		order:  []string{model.Name},
	}, nil
}

func loadNamespaced(root string, manifests []*Manifest, opts Options) (*Workspace, error) {
	ws := &Workspace{Root: root, byName: map[string]*Namespace{}}

	byName := map[string]*Manifest{}
	for _, m := range manifests {
		if prev, dup := byName[norm(m.Name)]; dup {
			return nil, fmt.Errorf(
				"namespace %q is declared twice, at %s and %s", m.Name, prev.File, m.File)
		}
		byName[norm(m.Name)] = m
	}

	// Pass one: parse each namespace's own models in isolation.
	for _, m := range manifests {
		ns := &Namespace{Name: m.Name, Manifest: m}
		ws.byName[norm(m.Name)] = ns
		ws.order = append(ws.order, m.Name)

		files, err := m.ModelFiles()
		if err != nil {
			ns.Err = err
			continue
		}
		model, err := osi.LoadFiles(files)
		if err != nil {
			ns.Err = fmt.Errorf("namespace %q:\n%w", m.Name, err)
			continue
		}
		// Stamp ownership before anything resolves, so every field can report
		// the namespace that governs it.
		for _, d := range model.Datasets {
			d.Namespace = m.Name
		}
		ns.Model = model
		ns.Digest = model.Version
	}

	// Pass two: graft imported datasets in, then resolve. Imports run after
	// every namespace has parsed, because an import must be checked against the
	// exporter's declared surface and the exporter may appear later on disk.
	for _, name := range ws.order {
		ns := ws.byName[norm(name)]
		if ns.Err != nil {
			continue
		}
		if err := ws.graftImports(ns); err != nil {
			ns.Err = err
			continue
		}
		schema, err := resolve.New(ns.Model)
		if err != nil {
			ns.Err = fmt.Errorf("namespace %q:\n%w", ns.Name, err)
			continue
		}
		ns.Schema = schema

		// After the schema, because every rollup is validated against it: a
		// rollup naming a metric that no longer exists has to fail here
		// rather than on the first caller who would have been routed to it.
		if ns.Manifest != nil {
			rollups, err := rollup.Load(filepath.Join(ns.Manifest.Dir, rollup.FileName), schema)
			if err != nil {
				ns.Err = fmt.Errorf("namespace %q rollups:\n%w", ns.Name, err)
				continue
			}
			ns.Rollups = rollups
		}
	}

	ws.Digest = workspaceDigest(ws)

	if opts.Strict {
		var failed []string
		for _, name := range ws.order {
			if ns := ws.byName[norm(name)]; ns.Err != nil {
				failed = append(failed, ns.Err.Error())
			}
		}
		if len(failed) > 0 {
			return ws, fmt.Errorf("%s", strings.Join(failed, "\n\n"))
		}
	}
	return ws, nil
}

// graftImports copies each imported dataset into the importing namespace under
// its local name, after checking the exporter allows it.
func (ws *Workspace) graftImports(ns *Namespace) error {
	if ns.Manifest == nil {
		return nil
	}
	for _, imp := range ns.Manifest.Imports {
		source, ok := ws.byName[norm(imp.Namespace)]
		if !ok {
			return fmt.Errorf("namespace %q imports from %q, which does not exist",
				ns.Name, imp.Namespace)
		}
		if source.Err != nil {
			return fmt.Errorf(
				"namespace %q imports from %q, which failed to load", ns.Name, imp.Namespace)
		}
		for _, item := range imp.Datasets {
			if !source.Manifest.exportsDataset(item.Name) {
				return fmt.Errorf(
					"namespace %q imports %s.%s, but %q does not export it; "+
						"add it to `exports.datasets` in %s, or stop importing it",
					ns.Name, imp.Namespace, item.Name, imp.Namespace, source.Manifest.File)
			}
			src, ok := source.Model.Dataset(item.Name)
			if !ok {
				return fmt.Errorf(
					"namespace %q imports %s.%s, which %q exports but does not define",
					ns.Name, imp.Namespace, item.Name, imp.Namespace)
			}
			local := item.LocalName()
			if _, clash := ns.Model.Dataset(local); clash {
				return fmt.Errorf(
					"namespace %q already defines a dataset named %q, so the import of %s.%s collides; "+
						"alias it with `as` in %s",
					ns.Name, local, imp.Namespace, item.Name, ns.Manifest.File)
			}
			ns.Model.Datasets = append(ns.Model.Datasets, cloneDatasetAs(src, local, ns.Name))
		}
	}
	return nil
}

// workspaceDigest hashes the sorted namespace digests. It is the version a
// query response and an audit event carry, and it is what makes "which
// definitions were in production" answerable from one value.
func workspaceDigest(ws *Workspace) string {
	pairs := make([]string, 0, len(ws.order))
	for _, name := range ws.order {
		ns := ws.byName[norm(name)]
		pairs = append(pairs, name+"="+ns.Digest)
	}
	sort.Strings(pairs)
	sum := sha256.Sum256([]byte(strings.Join(pairs, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

func norm(s string) string { return strings.ToUpper(s) }

// Namespaces returns every namespace in declaration order, including failed
// ones.
func (ws *Workspace) Namespaces() []*Namespace {
	out := make([]*Namespace, 0, len(ws.order))
	for _, name := range ws.order {
		out = append(out, ws.byName[norm(name)])
	}
	return out
}

// Available returns only the namespaces that loaded successfully.
func (ws *Workspace) Available() []*Namespace {
	var out []*Namespace
	for _, ns := range ws.Namespaces() {
		if ns.Available() {
			out = append(out, ns)
		}
	}
	return out
}

// Failed returns the namespaces that did not load.
func (ws *Workspace) Failed() []*Namespace {
	var out []*Namespace
	for _, ns := range ws.Namespaces() {
		if !ns.Available() {
			out = append(out, ns)
		}
	}
	return out
}

// Namespace looks one up by name, case-insensitively.
func (ws *Workspace) Namespace(name string) (*Namespace, bool) {
	ns, ok := ws.byName[norm(name)]
	return ns, ok
}

// Single returns the only namespace when there is exactly one available, which
// is what lets a single-team workspace accept unqualified names.
func (ws *Workspace) Single() (*Namespace, bool) {
	avail := ws.Available()
	if len(avail) == 1 {
		return avail[0], true
	}
	return nil, false
}

// Qualify prefixes a name with its namespace.
func Qualify(namespace, name string) string { return namespace + "." + name }
