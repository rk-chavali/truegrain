// Package config reads the project file a model repository carries.
//
// `truegrain.yaml` lives at the root of the repository that holds the models,
// not in this one. It is what lets a model repository be self-describing: point
// the binary at the directory and it knows which warehouse, which policy and
// which audit sink, without five flags on every command.
//
// Secrets never live in this file. It is committed, so any value that needs to
// stay out of version control is written as `${VAR}` and read from the
// environment, and an unset variable fails the load rather than silently
// becoming an empty string.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the conventional name, looked for automatically.
const FileName = "truegrain.yaml"

// Project is the parsed project file. Every path in it is resolved relative to
// the file's own directory, so the repository can be cloned anywhere.
type Project struct {
	// Dir is the directory holding the file.
	Dir string `yaml:"-"`
	// File is the file's path, for diagnostics.
	File string `yaml:"-"`

	Version   int       `yaml:"version"`
	Workspace Workspace `yaml:"workspace"`
	Warehouse Warehouse `yaml:"warehouse"`
	Govern    Govern    `yaml:"governance"`
	Audit     Audit     `yaml:"audit"`

	// Environments are named overlays on everything above: dev points at a
	// scratch database, prod points at the warehouse and a stricter policy,
	// and the model files are identical in both.
	//
	// Held as raw nodes rather than as []Project so that an overlay sets
	// only the fields it mentions. Decoding a node onto an already-populated
	// struct leaves absent fields alone, which is exactly the merge rule
	// wanted here and is free. A typed overlay would have to distinguish
	// "max_concurrent_queries: 0" from "not set", and the version of this
	// that guesses is the version that silently uncaps production.
	Environments map[string]yaml.Node `yaml:"environments"`

	// Environment is the overlay that was applied, empty when none was.
	// Reported by health and stamped on every audit event: the failure this
	// exists to prevent is somebody reading a number and not knowing which
	// environment produced it.
	Environment string `yaml:"-"`
}

// Use applies a named environment overlay.
//
// An unknown name is an error rather than a fall back to the base
// configuration. `-env prd` is a typo somebody will make, and serving
// development data under the belief that it is production, or the reverse,
// is the worst available outcome: both answer, and only one is right.
func (p *Project) Use(name string) error {
	if name == "" {
		if len(p.Environments) > 0 {
			return fmt.Errorf(
				"%s declares environments (%s) and none was chosen. Pass -env or set "+
					"TRUEGRAIN_ENV: with more than one configuration in the file, "+
					"defaulting to the base one means nobody can tell which answered",
				p.File, strings.Join(sortedKeys(p.Environments), ", "))
		}
		return nil
	}
	node, ok := p.Environments[name]
	if !ok {
		if len(p.Environments) == 0 {
			return fmt.Errorf("%s declares no environments, so -env %s names nothing",
				p.File, name)
		}
		return fmt.Errorf("%s has no environment %q; it declares %s",
			p.File, name, strings.Join(sortedKeys(p.Environments), ", "))
	}

	// An overlay declaring its own environments block would replace the map
	// it was read from, which reads as ordinary configuration and behaves
	// as a trapdoor: the next lookup would resolve against a different set
	// than the one the operator read in the file. Refused before it can.
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "environments" {
			return fmt.Errorf(
				"%s: environment %q declares its own environments block. An overlay "+
					"cannot change which environments exist", p.File, name)
		}
	}

	// Decoded onto the already-populated project, so an overlay that
	// mentions only warehouse.database leaves the dialect, the policy and
	// everything else as the base set them.
	if err := node.Decode(p); err != nil {
		return fmt.Errorf("%s: environment %q: %w", p.File, name, err)
	}
	p.Environment = name
	return p.validate()
}

func sortedKeys(m map[string]yaml.Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Workspace configures how namespaces are discovered.
type Workspace struct {
	// Discover lists globs matching namespace directories. Empty means walk
	// from the project root.
	Discover []string `yaml:"discover"`
}

// Warehouse names the compilation target and, for warehouses that are a local
// file, where that file is.
//
// No credential belongs in this struct. A key file path in a project file
// becomes a key file in a git repository, so BigQuery authenticates with
// Application Default Credentials and nothing else.
type Warehouse struct {
	Dialect string `yaml:"dialect"`
	// Database is the DuckDB file to execute against. Empty means the engine
	// compiles but does not execute, which is a complete configuration.
	Database string `yaml:"database"`

	// DSNEnv names the environment variable holding the PostgreSQL connection
	// string. The name, never the value: this file is committed, and the DSN
	// carries a password. Equivalent to -dsn-env, which wins when both are
	// given.
	DSNEnv string `yaml:"dsn_env"`

	// Project is the BigQuery project billed for queries.
	Project string `yaml:"project"`
	// Location pins jobs to a region, for example "US" or "europe-west2".
	// BigQuery cannot always infer it and the error when it cannot does not
	// mention the location.
	Location string `yaml:"location"`
	// MaxBytesBilled caps a single query, as a byte count. Zero applies the
	// executor's default; negative disables the cap.
	MaxBytesBilled int64 `yaml:"max_bytes_billed"`
	// MaxConcurrentQueries caps simultaneous warehouse execution across every
	// surface this process serves. Zero does not cap, and health says so.
	MaxConcurrentQueries int `yaml:"max_concurrent_queries"`
	// Impersonate runs each query as the calling identity rather than as this
	// process. Without it the warehouse cannot tell callers apart, so any row
	// or column security defined there applies to the engine's own service
	// account instead of to the user.
	Impersonate bool `yaml:"impersonate"`
}

// Govern points at the policy file.
type Govern struct {
	Policy string `yaml:"policy"`
}

// Audit configures where decisions are recorded.
type Audit struct {
	// Sink is one of none, stdout or file.
	Sink string `yaml:"sink"`
	Path string `yaml:"path"`
}

// Audit sink names.
const (
	SinkNone   = "none"
	SinkStdout = "stdout"
	SinkFile   = "file"
)

// Load reads a project file.
func Load(path string) (*Project, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Parse first, then substitute into string values only. Expanding the raw
	// text would also rewrite comments, so documenting the ${VAR} syntax inside
	// the file would fail the load.
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if missing := expandNode(&root); len(missing) > 0 {
		return nil, fmt.Errorf(
			"%s references environment variable(s) that are not set: %s\n"+
				"these are the values kept out of version control; set them before starting",
			path, strings.Join(dedupe(missing), ", "))
	}

	var p Project
	if err := root.Decode(&p); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	p.File = abs
	p.Dir = filepath.Dir(abs)

	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Discover looks for a project file in dir, then walks up to the filesystem
// root. Walking up is what lets a command run from inside a namespace
// directory, which is where someone editing a model actually is.
func Discover(dir string) (string, bool) {
	cur, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		candidate := filepath.Join(cur, FileName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		cur = parent
	}
}

func (p *Project) validate() error {
	if p.Version != 1 {
		return fmt.Errorf("%s: unsupported version %d, expected 1", p.File, p.Version)
	}
	switch p.Audit.Sink {
	case "", SinkNone, SinkStdout:
	case SinkFile:
		if p.Audit.Path == "" {
			return fmt.Errorf("%s: audit sink is `file` but no path is set", p.File)
		}
	default:
		return fmt.Errorf("%s: unknown audit sink %q, expected none, stdout or file",
			p.File, p.Audit.Sink)
	}
	return nil
}

// Resolve turns a path from the file into an absolute one. An empty value stays
// empty so a caller can tell "not configured" from "configured to the root".
func (p *Project) Resolve(path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.Dir, filepath.FromSlash(path))
}

// ModelsPath is the directory the workspace loads from, which is the project
// root itself.
func (p *Project) ModelsPath() string { return p.Dir }

// PolicyPath is the resolved policy file, or empty when none is configured.
func (p *Project) PolicyPath() string { return p.Resolve(p.Govern.Policy) }

// DatabasePath is the resolved warehouse file, or empty for compile-only.
func (p *Project) DatabasePath() string { return p.Resolve(p.Warehouse.Database) }

// AuditPath is the resolved audit file.
func (p *Project) AuditPath() string { return p.Resolve(p.Audit.Path) }

// envRef matches ${VAR}. The syntax is deliberately narrow: no defaults, no
// nested expansion, no shell semantics. A config file that can compute is a
// config file nobody can review.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandNode substitutes environment variables into every quoted or plain
// string in the document, returning the names of any that are unset.
//
// Failing on an unset variable is the point. One becoming an empty string is
// how a deployment silently connects to the wrong warehouse or loads no policy
// at all, and both are worse than not starting.
func expandNode(n *yaml.Node) []string {
	var missing []string
	if n.Kind == yaml.ScalarNode && (n.Tag == "!!str" || n.Tag == "") {
		n.Value = envRef.ReplaceAllStringFunc(n.Value, func(match string) string {
			name := envRef.FindStringSubmatch(match)[1]
			value, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
				return match
			}
			return value
		})
	}
	for _, child := range n.Content {
		missing = append(missing, expandNode(child)...)
	}
	return missing
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
