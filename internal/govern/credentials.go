package govern

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Credentials is a bearer token file that reloads when it changes.
//
// This exists so a token can be rotated without a restart, and the shape is
// dictated by the only rotation that actually works: overlap. An operator
// adds the new token, rolls the clients onto it, then removes the old one.
// Every other scheme has a window where one side has rotated and the other
// has not, and that window is an outage.
//
// A token also carries an optional not_after. Removing an entry is how a
// credential ends, but an operator planning a rotation wants the old one to
// die on a date whether or not anybody remembers to edit the file. Without
// it, "we rotated in March" means the March token is still valid today.
//
// This is deliberately not the environment variable that -token-env reads.
// An environment variable is fixed for the life of the process, so rotating
// one means a restart, which means a deployment, which is why nobody
// rotates. A file is what a platform secret mount updates in place:
// Kubernetes rewrites the mounted secret and this notices within seconds.
//
// The file holds live credentials and is not a thing to commit. It is the
// same category as a kubeconfig: mounted from the platform secret manager,
// readable only by the process. Nothing here logs a token or puts one in an
// error; entries carry an id precisely so a log line can name which
// credential was used without naming the credential.
//
// ponytail: the file stores tokens in the clear rather than hashed. It is
// already a secret mount, so hashing protects against a reader who can
// already read the mount. Store a hash instead if the file ever has to live
// somewhere less private than the process that reads it.
type Credentials struct {
	path string

	mu      sync.RWMutex
	tokens  []credential
	modTime time.Time
	size    int64
	checked time.Time
}

// credential is one entry, parsed.
type credential struct {
	// ID names the credential in a log. Never the secret.
	ID string `yaml:"id"`
	// Token is the bearer value a client sends.
	Token string `yaml:"token"`
	// Subject, Groups and Scopes are the identity this token authenticates
	// as, the same three fields every other authenticator produces.
	Subject string   `yaml:"subject"`
	Groups  []string `yaml:"groups"`
	Scopes  []string `yaml:"scopes"`
	// NotAfter is when this token stops working, whether or not anyone
	// remembers to delete it. Zero means no expiry.
	NotAfter time.Time `yaml:"not_after"`
}

type credentialFile struct {
	Version int          `yaml:"version"`
	Tokens  []credential `yaml:"tokens"`
}

// LoadCredentials reads the token file and returns something that keeps it
// current.
//
// A missing file at startup is an error. Pointing at a path that is not
// there is a typo, and treating it as "no tokens" would start a server that
// refuses every caller and looks like an authentication bug.
func LoadCredentials(path string) (*Credentials, error) {
	c := &Credentials{path: path}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// Lookup resolves a presented token to the identity it authenticates as.
//
// Comparison is constant time and scans every entry rather than looking one
// up, for the same reason rest.StaticTokens does: a map answers faster for a
// miss than for a hit, and over enough requests that is a way to learn a
// token a byte at a time. A token file holds a handful of entries, so the
// scan costs nothing.
//
// An expired entry is skipped rather than matched-and-rejected. A caller
// presenting a token that has passed its not_after gets "unrecognised",
// which is the same answer an unknown token gets, because telling them the
// difference tells them the token was once real.
func (c *Credentials) Lookup(token string) (Identity, bool) {
	c.maybeReload()

	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	var found Identity
	var matched bool
	for _, entry := range c.tokens {
		if !entry.NotAfter.IsZero() && now.After(entry.NotAfter) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(entry.Token), []byte(token)) == 1 {
			found = Identity{
				Subject: entry.Subject,
				Groups:  entry.Groups,
				Scopes:  entry.Scopes,
				TokenID: entry.ID,
			}
			matched = true
		}
	}
	return found, matched
}

// Active reports how many entries are usable right now, for a health
// endpoint. An operator who has rotated wants to see the count go from two
// back to one rather than reading the file to check.
func (c *Credentials) Active() int {
	c.maybeReload()

	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, entry := range c.tokens {
		if entry.NotAfter.IsZero() || now.Before(entry.NotAfter) {
			n++
		}
	}
	return n
}

// Path is where the credentials came from, for a health endpoint. The path,
// never the contents.
func (c *Credentials) Path() string { return c.path }

func (c *Credentials) reload() error {
	info, err := os.Stat(c.path)
	if err != nil {
		return fmt.Errorf("reading the credentials file %s: %w", c.path, err)
	}

	c.mu.RLock()
	unchanged := info.ModTime().Equal(c.modTime) && info.Size() == c.size && c.tokens != nil
	c.mu.RUnlock()
	if unchanged {
		return nil
	}

	src, err := os.ReadFile(c.path)
	if err != nil {
		return fmt.Errorf("reading the credentials file %s: %w", c.path, err)
	}

	var file credentialFile
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		// The error carries the parser's message, which names a line. It
		// cannot carry a value, because a value here is a token.
		return fmt.Errorf("parsing %s: %w", c.path, err)
	}
	if file.Version != 1 {
		return fmt.Errorf("%s: unsupported version %d, expected 1", c.path, file.Version)
	}
	if len(file.Tokens) == 0 {
		return fmt.Errorf(
			"%s lists no tokens; a file with none would refuse every caller and "+
				"read as an authentication bug rather than as configuration", c.path)
	}

	seen := map[string]int{}
	for i, entry := range file.Tokens {
		switch {
		case entry.ID == "":
			return fmt.Errorf(
				"%s: token %d has no id. The id is what a log line names when this "+
					"credential is used, and the alternative is naming the token",
				c.path, i+1)
		case entry.Token == "":
			return fmt.Errorf("%s: token %q has no value", c.path, entry.ID)
		case entry.Subject == "":
			return fmt.Errorf(
				"%s: token %q authenticates as nobody. Give it a subject, or the "+
					"policy has no identity to apply", c.path, entry.ID)
		}
		if at, duplicate := seen[entry.ID]; duplicate {
			return fmt.Errorf("%s: tokens %d and %d share the id %q; an id has to "+
				"identify one credential", c.path, at+1, i+1, entry.ID)
		}
		seen[entry.ID] = i
	}

	// Two entries sharing a token value would make which identity a caller
	// gets depend on iteration order. During a rotation the two entries are
	// supposed to hold different values; sharing one is a copy-paste that
	// silently grants the wrong identity.
	for i := range file.Tokens {
		for j := i + 1; j < len(file.Tokens); j++ {
			if file.Tokens[i].Token == file.Tokens[j].Token {
				return fmt.Errorf(
					"%s: tokens %q and %q hold the same value, so which identity a "+
						"caller gets would depend on file order",
					c.path, file.Tokens[i].ID, file.Tokens[j].ID)
			}
		}
	}

	c.mu.Lock()
	c.tokens = file.Tokens
	c.modTime, c.size = info.ModTime(), info.Size()
	c.checked = time.Now()
	c.mu.Unlock()
	return nil
}

// maybeReload re-reads the file when it has changed, at most every few
// seconds.
//
// A read failure after startup is deliberately not fatal and deliberately
// does not clear the list. A secret mount is briefly inconsistent while the
// platform swaps it, and a server that refused every caller during that
// window would turn a routine rotation into an outage. The last good list
// keeps applying, which is the conservative direction: it can serve a token
// a few seconds past its removal, and it cannot lock everybody out.
//
// The one thing that is not deferred is not_after, which is evaluated
// against the clock on every lookup rather than at load. An expiry that
// only took effect on the next successful reload would not take effect at
// all if the file stopped being readable.
func (c *Credentials) maybeReload() {
	c.mu.RLock()
	fresh := time.Since(c.checked) < reloadInterval
	c.mu.RUnlock()
	if fresh {
		return
	}
	c.mu.Lock()
	c.checked = time.Now()
	c.mu.Unlock()
	_ = c.reload()
}

// ErrUnrecognised is what a failed lookup becomes at an authentication
// boundary. One message for every failure: an unknown token, an expired
// one and a malformed header are indistinguishable to the caller, because
// the difference between them is exactly what a prober is looking for.
var ErrUnrecognised = errors.New("unrecognised credentials")
