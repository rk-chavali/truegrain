// Package workspace composes several independently owned Ossie models into one
// engine without merging them into a single flat namespace.
//
// The problem it solves: a directory of Ossie files is either one model, in
// which case every team shares one global namespace and collides, or several
// models, which the loader refuses. Neither works once more than one team owns
// definitions.
//
// The shape: each team's directory carries its own `namespace.yaml`. Nothing is
// shared, so there is no root file for twelve teams to contend on. Composition
// metadata lives only in these manifests; the model files beside them stay
// stock Ossie documents that any other OSI tool can read.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rk-chavali/truegrain/internal/rollup"

	"github.com/rk-chavali/truegrain/internal/osi"
)

// ManifestFile is the reserved filename. Every other YAML file in a namespace
// directory is parsed as an Ossie model.
const ManifestFile = "namespace.yaml"

// Manifest is one namespace's declaration of itself.
type Manifest struct {
	// Dir is the directory holding the manifest, filled in at load time.
	Dir string `yaml:"-"`
	// File is the manifest's own path, for diagnostics.
	File string `yaml:"-"`

	Version     int      `yaml:"version"`
	Name        string   `yaml:"name"`
	Owners      []string `yaml:"owners"`
	Description string   `yaml:"description"`

	// Exports lists what other namespaces may reference. Everything not listed
	// here is private, so a team can change its internals without breaking a
	// consumer it does not know about.
	Exports Exports `yaml:"exports"`

	// Imports lists what this namespace reaches into. Both sides opt in, which
	// is what makes the dependency graph readable from the manifests alone.
	Imports []Import `yaml:"imports"`
}

// Exports is a namespace's public surface.
//
// Only datasets are exportable. A dataset is the unit another namespace can
// join to, and a metric belongs to the datasets it aggregates, so exporting one
// across a namespace boundary would have nothing to bind to.
type Exports struct {
	Datasets []string `yaml:"datasets"`
}

// Import is a dependency on one other namespace.
type Import struct {
	Namespace string       `yaml:"namespace"`
	Datasets  []ImportItem `yaml:"datasets"`
}

// ImportItem names an imported dataset, optionally aliasing it.
//
// It accepts either a bare string or a mapping, so the common case stays short:
//
//	datasets: [orders]
//	datasets: [{name: orders, as: sales_orders}]
type ImportItem struct {
	Name string `yaml:"name"`
	As   string `yaml:"as"`
}

// LocalName is the name the imported dataset takes inside the importing
// namespace.
func (i ImportItem) LocalName() string {
	if i.As != "" {
		return i.As
	}
	return i.Name
}

func (i *ImportItem) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		i.Name = n.Value
		return nil
	}
	type plain ImportItem
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*i = ImportItem(p)
	return nil
}

// LoadManifest reads and validates one namespace manifest.
func LoadManifest(path string) (*Manifest, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(src, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	m.File = path
	m.Dir = filepath.Dir(path)

	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// validate checks a manifest in isolation. Cross-namespace checks, such as
// whether an imported dataset is actually exported, happen once every manifest
// is known.
func (m *Manifest) validate() error {
	if m.Version != 1 {
		return fmt.Errorf("%s: unsupported version %d, expected 1", m.File, m.Version)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("%s: a namespace must declare a name", m.File)
	}
	if !validName(m.Name) {
		return fmt.Errorf(
			"%s: namespace name %q must be a bare identifier: letters, digits and underscores, starting with a letter",
			m.File, m.Name)
	}
	if len(m.Owners) == 0 {
		// An unowned namespace has nobody to approve a grant against it and
		// nobody to ask when a metric looks wrong. Refuse rather than warn.
		return fmt.Errorf(
			"%s: namespace %q declares no owners; an owner is who approves access to it and who answers when a number is disputed",
			m.File, m.Name)
	}

	seenExport := map[string]bool{}
	for _, d := range m.Exports.Datasets {
		if seenExport[d] {
			return fmt.Errorf("%s: dataset %q is exported twice", m.File, d)
		}
		seenExport[d] = true
	}

	seenImportNS := map[string]bool{}
	localNames := map[string]string{}
	for _, imp := range m.Imports {
		if imp.Namespace == "" {
			return fmt.Errorf("%s: an import declares no namespace", m.File)
		}
		if imp.Namespace == m.Name {
			return fmt.Errorf("%s: namespace %q imports from itself", m.File, m.Name)
		}
		if seenImportNS[imp.Namespace] {
			return fmt.Errorf(
				"%s: namespace %q is imported twice; combine them into one entry", m.File, imp.Namespace)
		}
		seenImportNS[imp.Namespace] = true

		if len(imp.Datasets) == 0 {
			return fmt.Errorf(
				"%s: the import of %q lists no datasets", m.File, imp.Namespace)
		}
		for _, item := range imp.Datasets {
			if item.Name == "" {
				return fmt.Errorf("%s: an imported dataset from %q has no name", m.File, imp.Namespace)
			}
			local := item.LocalName()
			if prev, dup := localNames[local]; dup {
				return fmt.Errorf(
					"%s: %q and %q both arrive as %q; alias one of them with `as`",
					m.File, prev, imp.Namespace+"."+item.Name, local)
			}
			localNames[local] = imp.Namespace + "." + item.Name
		}
	}
	return nil
}

// Exports reports whether this namespace exports the named dataset.
func (m *Manifest) exportsDataset(name string) bool {
	for _, d := range m.Exports.Datasets {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

// ModelFiles lists the Ossie model files in the namespace directory, which is
// every YAML file except the manifest itself.
func (m *Manifest) ModelFiles() ([]string, error) {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		return nil, fmt.Errorf("reading namespace directory %s: %w", m.Dir, err)
	}
	var out []string
	for _, e := range entries {
		// The rollup sidecar sits in the same directory and is not an
		// Ossie model. Reading it as one fails the whole namespace with a
		// parser error about a file the author never meant to be a model.
		if e.IsDir() || e.Name() == ManifestFile || e.Name() == rollup.FileName {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
			out = append(out, filepath.Join(m.Dir, e.Name()))
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("namespace %q at %s contains no Ossie model files", m.Name, m.Dir)
	}
	return out, nil
}

// validName reports whether s is usable as a namespace name. The constraint is
// the Ossie identifier rule, because a namespace name is prefixed onto object
// names at the API boundary and must survive being one.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// cloneDatasetAs produces an independent copy of a dataset under a new name, so
// an imported dataset can be addressed by a local name without the exporting
// namespace seeing the rename.
//
// Fields are copied because each carries a back-reference to its dataset, and
// leaving those pointing at the original would make a qualified name resolve
// into the wrong namespace.
func cloneDatasetAs(d *osi.Dataset, name, intoNamespace string) *osi.Dataset {
	clone := *d
	clone.Name = name
	clone.Namespace = intoNamespace
	// Record where it came from. Governance resolves against this, so aliasing
	// an import cannot detach a column from the grant written against it.
	clone.ImportedFrom = d.Origin()
	clone.Fields = make([]*osi.Field, len(d.Fields))
	for i, f := range d.Fields {
		fc := *f
		fc.Dataset = &clone
		clone.Fields[i] = &fc
	}
	return &clone
}
