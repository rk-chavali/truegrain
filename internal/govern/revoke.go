package govern

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Turning off a credential before it expires.
//
// A bearer token is valid until its expiry and nothing could shorten that,
// which meant the answer to "a token leaked" was to wait. For an OIDC
// deployment with an hour-long token that is an hour of somebody else
// reading the warehouse; for a shared token with no expiry at all it was
// "restart the process", assuming anybody knew which one.
//
// Two levers, because incidents come in two shapes.
//
//	jti      one credential, when the issuer mints an identifier for it
//	subject  every credential a workload holds
//
// The subject lever is the one that always works and the one nobody wants to
// pull, because it takes the workload offline. The jti lever is the one that
// gets used, and it only exists if the issuer sets the claim; a deployment
// whose issuer does not is told so rather than left believing it has a lever
// it does not.
//
// # Why a file
//
// A revocation has to take effect without a restart, or it is not a
// revocation: an operator will not take a production engine down to turn off
// one token, and an engine that needs restarting to honour a deny list will
// be restarted late. The file is re-read when it changes, so revoking is
// editing a file that is already in configuration management.
//
// Deliberately not a database and not an API. A revocation endpoint is a
// write path into a process that has none, and it would need its own
// authentication, which is the thing currently in question. Git and a
// deployment pipeline already solve this.

// Revocation is one entry in the deny list.
type Revocation struct {
	// Subject revokes every credential this identity presents.
	Subject string `yaml:"subject,omitempty"`
	// JTI revokes one credential.
	JTI string `yaml:"jti,omitempty"`
	// Reason is for whoever reads the file in six months.
	Reason string `yaml:"reason,omitempty"`
	// At records when, for the same reason.
	At string `yaml:"at,omitempty"`
}

type revocationFile struct {
	Version int          `yaml:"version"`
	Revoked []Revocation `yaml:"revoked"`
}

// Revocations is a deny list that reloads when its file changes.
type Revocations struct {
	path string

	mu       sync.RWMutex
	subjects map[string]Revocation
	jtis     map[string]Revocation
	// modTime and size together decide whether to re-read. Both, because a
	// file rewritten within the same filesystem timestamp granularity is
	// exactly what a scripted revocation looks like.
	modTime time.Time
	size    int64
	// checked throttles the stat, so a busy engine does not stat the file on
	// every single request.
	checked time.Time
}

// reloadInterval bounds how stale a revocation can be.
//
// Five seconds: short enough that an incident response is not waiting on it,
// long enough that the file is statted a handful of times a second at worst
// rather than once per request.
// A var rather than a const only so a test can shorten it: waiting out five
// seconds twice to prove a rotation works is eighteen seconds of CI for a
// property that has nothing to do with the interval.
var reloadInterval = 5 * time.Second

// LoadRevocations reads the deny list and returns something that keeps it
// current.
//
// A missing file at startup is an error rather than an empty list. Pointing
// at a path that is not there is a typo, and treating it as "nothing is
// revoked" is the failure mode where an operator believes revocation is
// configured and it is not.
func LoadRevocations(path string) (*Revocations, error) {
	r := &Revocations{path: path}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Revocations) reload() error {
	info, err := os.Stat(r.path)
	if err != nil {
		return fmt.Errorf("reading the revocation list %s: %w", r.path, err)
	}

	r.mu.RLock()
	unchanged := info.ModTime().Equal(r.modTime) && info.Size() == r.size && r.subjects != nil
	r.mu.RUnlock()
	if unchanged {
		return nil
	}

	src, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("reading the revocation list %s: %w", r.path, err)
	}

	var file revocationFile
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return fmt.Errorf("parsing %s: %w", r.path, err)
	}
	if file.Version != 1 {
		return fmt.Errorf("%s: unsupported version %d, expected 1", r.path, file.Version)
	}

	subjects := map[string]Revocation{}
	jtis := map[string]Revocation{}
	for i, entry := range file.Revoked {
		switch {
		case entry.Subject == "" && entry.JTI == "":
			return fmt.Errorf(
				"%s: entry %d revokes nothing; give it a subject or a jti",
				r.path, i+1)
		case entry.Subject != "" && entry.JTI != "":
			// Both would read as "this jti, but only for this subject", which
			// is not what it does and would quietly revoke more than intended.
			return fmt.Errorf(
				"%s: entry %d sets both subject and jti. Use one per entry: a "+
					"subject revokes every credential that identity holds, a jti "+
					"revokes one", r.path, i+1)
		case entry.Subject != "":
			subjects[strings.ToLower(entry.Subject)] = entry
		default:
			jtis[entry.JTI] = entry
		}
	}

	r.mu.Lock()
	r.subjects, r.jtis = subjects, jtis
	r.modTime, r.size = info.ModTime(), info.Size()
	r.checked = time.Now()
	r.mu.Unlock()
	return nil
}

// maybeReload re-reads the file when it has changed, at most every few
// seconds.
//
// A read failure after startup is deliberately not fatal and deliberately
// does not clear the list: the file being briefly absent mid-edit must not
// un-revoke a leaked credential. The last good list keeps applying.
func (r *Revocations) maybeReload() {
	r.mu.RLock()
	fresh := time.Since(r.checked) < reloadInterval
	r.mu.RUnlock()
	if fresh {
		return
	}
	r.mu.Lock()
	r.checked = time.Now()
	r.mu.Unlock()
	_ = r.reload()
}

// Check reports the revocation covering this identity, if any.
func (r *Revocations) Check(id Identity) (Revocation, bool) {
	r.maybeReload()

	r.mu.RLock()
	defer r.mu.RUnlock()

	if id.TokenID != "" {
		if rev, ok := r.jtis[id.TokenID]; ok {
			return rev, true
		}
	}
	if rev, ok := r.subjects[strings.ToLower(id.Subject)]; ok {
		return rev, true
	}
	return Revocation{}, false
}

// Count reports how many entries are loaded, for health.
func (r *Revocations) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subjects) + len(r.jtis)
}

// Each serving surface wraps its own authenticator around this list rather
// than the list wrapping them, because the surfaces authenticate differently:
// REST reads a bearer header, the wire protocol reads a startup exchange.
// What they share is the list and the rule, and that is what lives here.
