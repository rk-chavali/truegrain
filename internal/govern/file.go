package govern

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rk-chavali/truegrain/internal/plan"
)

// FieldIndex resolves the names written in a policy file against the loaded
// workspace.
//
// It is an interface rather than a concrete workspace so that this package does
// not depend on composition. A policy is written against semantic names, and
// the only thing it needs from the workspace is the ability to turn a name a
// human typed into the canonical governed name.
type FieldIndex interface {
	// GovernedField canonicalises a policy entry. It accepts `dataset.field`
	// when unambiguous across the workspace and `namespace.dataset.field`
	// always. When the name is ambiguous it returns the candidates so the error
	// can name them; when it matches nothing it returns ok false.
	GovernedField(name string) (governed string, candidates []string, ok bool)
	// GovernedFieldsIn returns every governed field name in a namespace, which
	// is how a tag protects a whole namespace at once.
	GovernedFieldsIn(namespace string) ([]string, bool)
}

// FilePolicy resolves access from a policy file checked into the model
// repository alongside the model it governs. It must not live inside a
// namespace directory, which is loaded wholesale and contains only Ossie models.
//
// It exists for two reasons. It is the resolver the test suite can prove
// compile-time enforcement with, on any machine, with no cloud account. And it
// is the honest option for warehouses that have no column-level security of
// their own, where the engine's own gate is the only control available.
//
// It is not a substitute for warehouse-native policy tags where those exist.
// A file resolver governs what goes through this engine; it cannot stop anyone
// who can reach the warehouse directly. Capabilities says so.
type FilePolicy struct {
	path string
	// required maps a governed field name to the tags needed to read it.
	required map[string][]string
	// granted maps a principal to the set of tags it holds.
	granted map[string]map[string]bool
	// rows restrict which records a principal may see. Separate from the tag
	// machinery because it answers a different question and combining them
	// would make one file that is hard to read in either direction.
	rows RowPolicies
}

// Rows returns the row restrictions this policy declares.
func (p *FilePolicy) Rows() RowPolicies { return p.rows }

// policyFile is the on-disk shape.
type policyFile struct {
	Version int `yaml:"version"`
	Tags    []struct {
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Fields      []string `yaml:"fields"`
		// Namespaces protects every field in a namespace with this tag, which
		// is what makes a policy tractable once a workspace has hundreds of
		// metrics across a dozen teams.
		Namespaces []string `yaml:"namespaces"`
		// Except carves specific fields back out of a namespace-wide tag.
		Except []string `yaml:"except"`
	} `yaml:"tags"`
	Grants []struct {
		// Principal is a service account, a user or a group. Grants to groups
		// are preferred; per-human grants become unmanageable.
		Principal string   `yaml:"principal"`
		Tags      []string `yaml:"tags"`
	} `yaml:"grants"`
	// Rows restrict which records a principal may see, as opposed to which
	// columns. A namespace with no entry here shows every row, which matches
	// how an untagged column is readable.
	Rows []struct {
		Namespace   string `yaml:"namespace"`
		Principal   string `yaml:"principal"`
		Description string `yaml:"description"`
		Filter      struct {
			Dimension string `yaml:"dimension"`
			Op        string `yaml:"op"`
			Values    []any  `yaml:"values"`
		} `yaml:"filter"`
	} `yaml:"rows"`
}

// LoadFilePolicy reads a policy file and validates it against the workspace.
//
// Validation is strict on purpose. A field name in the policy that does not
// exist is almost always a typo or a rename, and the failure mode is a column
// everyone believed was protected being silently readable. That is a security
// bug, so it fails the load rather than warning.
func LoadFilePolicy(path string, idx FieldIndex) (*FilePolicy, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy file: %w", err)
	}
	var pf policyFile
	if err := yaml.Unmarshal(src, &pf); err != nil {
		return nil, fmt.Errorf("parsing policy file %s: %w", path, err)
	}
	if pf.Version != 1 {
		return nil, fmt.Errorf("policy file %s: unsupported version %d, expected 1", path, pf.Version)
	}

	p := &FilePolicy{
		path:     path,
		required: map[string][]string{},
		granted:  map[string]map[string]bool{},
	}

	for _, r := range pf.Rows {
		p.rows = append(p.rows, RowPolicy{
			Namespace:   r.Namespace,
			Principal:   r.Principal,
			Description: r.Description,
			Filter: plan.Filter{
				Dimension: r.Filter.Dimension,
				Op:        plan.Op(r.Filter.Op),
				Values:    r.Filter.Values,
			},
		})
	}

	known := map[string]bool{}
	for _, tag := range pf.Tags {
		if tag.Name == "" {
			return nil, fmt.Errorf("policy file %s: a tag has no name", path)
		}
		if known[tag.Name] {
			return nil, fmt.Errorf("policy file %s: duplicate tag %q", path, tag.Name)
		}
		known[tag.Name] = true
		if len(tag.Fields) == 0 && len(tag.Namespaces) == 0 {
			return nil, fmt.Errorf(
				"policy file %s: tag %q protects no fields and no namespaces", path, tag.Name)
		}

		// Namespace-wide protection first, so an explicit field entry cannot be
		// silently undone by a later `except`.
		excepted := map[string]bool{}
		for _, name := range tag.Except {
			governed, err := canonical(path, tag.Name, name, idx)
			if err != nil {
				return nil, err
			}
			excepted[governed] = true
		}
		for _, nsName := range tag.Namespaces {
			fields, ok := idx.GovernedFieldsIn(nsName)
			if !ok {
				return nil, fmt.Errorf(
					"policy file %s: tag %q protects namespace %q, which the workspace does not define",
					path, tag.Name, nsName)
			}
			for _, governed := range fields {
				if !excepted[governed] {
					p.required[governed] = appendTag(p.required[governed], tag.Name)
				}
			}
		}
		for _, name := range tag.Fields {
			governed, err := canonical(path, tag.Name, name, idx)
			if err != nil {
				return nil, err
			}
			p.required[governed] = appendTag(p.required[governed], tag.Name)
		}
	}

	for _, g := range pf.Grants {
		if g.Principal == "" {
			return nil, fmt.Errorf("policy file %s: a grant has no principal", path)
		}
		set := p.granted[g.Principal]
		if set == nil {
			set = map[string]bool{}
			p.granted[g.Principal] = set
		}
		for _, tag := range g.Tags {
			if !known[tag] {
				return nil, fmt.Errorf(
					"policy file %s: grant to %q names unknown tag %q", path, g.Principal, tag)
			}
			set[tag] = true
		}
	}
	return p, nil
}

// canonical turns a name written in a policy file into the governed name,
// failing loudly on anything it cannot pin to exactly one field.
func canonical(path, tag, name string, idx FieldIndex) (string, error) {
	governed, candidates, ok := idx.GovernedField(name)
	if ok {
		return governed, nil
	}
	if len(candidates) > 0 {
		sort.Strings(candidates)
		return "", fmt.Errorf(
			"policy file %s: tag %q names %q, which is ambiguous across namespaces (%s); "+
				"qualify it as namespace.dataset.field",
			path, tag, name, strings.Join(candidates, ", "))
	}
	return "", fmt.Errorf(
		"policy file %s: tag %q protects %q, which the workspace does not define; "+
			"a stale policy entry leaves a column unprotected, so this is refused rather than ignored",
		path, tag, name)
}

func appendTag(tags []string, tag string) []string {
	for _, t := range tags {
		if t == tag {
			return tags
		}
	}
	return append(tags, tag)
}

// CanRead denies a ref when the identity lacks any tag protecting it.
//
// All required tags must be held, not any. A column carrying two
// classifications is protected by both, and satisfying one of them is not
// permission to read it.
func (p *FilePolicy) CanRead(_ context.Context, id Identity, refs []Ref) (Decision, error) {
	held := map[string]bool{}
	for _, principal := range id.principals() {
		for tag := range p.granted[principal] {
			held[tag] = true
		}
	}

	var denied []Ref
	for _, r := range refs {
		for _, tag := range p.required[r.Name()] {
			if !held[tag] {
				denied = append(denied, r)
				break
			}
		}
	}
	return Decision{Allowed: len(denied) == 0, Denied: denied}, nil
}

// Capabilities states what a file-backed policy does and does not guarantee.
func (p *FilePolicy) Capabilities() Capabilities {
	return Capabilities{
		Resolver:    "file",
		ColumnLevel: true,
		Note: "Column access is resolved from " + p.path + " before SQL is emitted. " +
			"This governs queries that go through this engine only. It places no " +
			"control on the warehouse itself, so a caller with direct warehouse " +
			"credentials is unaffected by it.",
	}
}

// ProtectedFields lists every governed field and the tags it requires, for
// operators checking what a policy actually covers.
func (p *FilePolicy) ProtectedFields() map[string][]string {
	out := make(map[string][]string, len(p.required))
	for field, tags := range p.required {
		sorted := append([]string(nil), tags...)
		sort.Strings(sorted)
		out[field] = sorted
	}
	return out
}

// String summarises the policy for the CLI.
func (p *FilePolicy) String() string {
	fields := p.ProtectedFields()
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)
	return fmt.Sprintf("file policy %s: %d protected field(s): %s",
		p.path, len(names), strings.Join(names, ", "))
}
