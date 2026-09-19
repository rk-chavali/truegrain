// Package gitsync keeps a local copy of a model repository in step with git.
//
// This is what makes a self-hosted engine deployable the way a team already
// works: an analyst opens a pull request, CI says which numbers it moves, it
// merges, and the engine picks it up. Without it the only way a model reaches
// a running engine is somebody copying files onto a volume, which is the step
// where the thing serving stops matching the thing reviewed.
//
// Pull rather than push, deliberately. A self-hosted engine usually sits
// inside a network that a CI runner cannot reach, so a push-based deploy would
// mean punching a hole inwards and handing CI a credential to the inside. This
// reverses it: nothing needs a route in, and the credential that reaches the
// warehouse never leaves the customer's network. It is the same reason ArgoCD
// reconciles from inside a cluster rather than being deployed into.
//
// Implemented with go-git rather than by running `git`. The shipped image is
// distroless and contains exactly one binary; there is no `git` in it and
// adding one would trade away the reason the image is that small.
package gitsync

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Prefix marks a -models value as a repository rather than a path.
//
// Spelled `git+https://` rather than accepting a bare `https://` URL, because
// a bare URL is ambiguous with a path on Windows and, more importantly,
// because an operator typing it should have to say what they mean.
const Prefix = "git+"

// Source is a repository, a ref, and the directory inside it holding models.
type Source struct {
	// URL is the clone URL, without credentials. See Parse.
	URL string
	// Ref is a branch, tag or commit. Empty follows the remote's default
	// branch, which is what a team merging to main wants.
	Ref string
	// Subdir is the path inside the repository holding the workspace, empty
	// for the repository root.
	Subdir string
	// TokenEnv names the environment variable holding a token for a private
	// repository. Named, never the value: a token on a command line is in the
	// process list and in shell history.
	TokenEnv string
	// Dir is where the working copy is kept between polls.
	Dir string
}

// Parse reads a `git+` prefixed -models value.
//
// The accepted shape is
//
//	git+https://host/org/repo.git#ref:subdir
//
// with both `ref` and `subdir` optional. The fragment carries them rather than
// a query string so the value survives being pasted into a shell without
// quoting, which is where `&` would otherwise split the command.
func Parse(value string) (Source, error) {
	rest, ok := strings.CutPrefix(value, Prefix)
	if !ok {
		return Source{}, fmt.Errorf("%q is not a git source; it should start with %q", value, Prefix)
	}

	var fragment string
	if base, frag, found := strings.Cut(rest, "#"); found {
		rest, fragment = base, frag
	}

	parsed, err := url.Parse(rest)
	if err != nil {
		return Source{}, fmt.Errorf("the repository URL could not be read: %w", err)
	}

	// Scheme allowlist rather than a denylist. `file://` would let a -models
	// value reach anything the process can read, and plain `git://` is
	// unauthenticated and unencrypted, so a model could be swapped in transit
	// by anything on the path.
	switch parsed.Scheme {
	case "https", "ssh":
	case "":
		return Source{}, errors.New("the repository URL needs a scheme, https or ssh")
	default:
		return Source{}, fmt.Errorf(
			"%q repositories are not read; use https or ssh. "+
				"Plain git:// carries no authentication and no encryption, so what arrives "+
				"is whatever the network chose to hand over", parsed.Scheme)
	}

	// A credential in the URL ends up in every log line, every error, and the
	// process list. Refused rather than scrubbed, because scrubbing is a thing
	// that works until one path forgets to.
	//
	// A bare username is not a credential, and over-refusing one would reject
	// `ssh://git@github.com/...`, which is how every SSH remote on earth is
	// written. So: a password is refused everywhere, and a username is refused
	// only over https, where in practice it is a token wearing a username.
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return Source{}, errors.New(
				"the repository URL carries a password. Remove it and name an environment " +
					"variable holding the token instead: a credential in a URL reaches " +
					"every log line and the process list")
		}
		if parsed.Scheme == "https" {
			return Source{}, errors.New(
				"the repository URL carries a username, which over https is almost always " +
					"a token. Remove it and use -git-token-env to name the variable holding " +
					"it: a credential in a URL reaches every log line and the process list")
		}
	}

	src := Source{URL: parsed.String()}
	if ref, subdir, found := strings.Cut(fragment, ":"); found {
		src.Ref, src.Subdir = ref, subdir
	} else {
		src.Ref = fragment
	}
	if err := validateSubdir(src.Subdir); err != nil {
		return Source{}, err
	}
	return src, nil
}

// validateSubdir refuses a path that would resolve outside the clone.
//
// The value is operator supplied rather than caller supplied, so this is not
// the front line of anything, but a `..` here reads files outside the
// repository and the check costs one function.
func validateSubdir(subdir string) error {
	if subdir == "" {
		return nil
	}
	if filepath.IsAbs(subdir) || strings.HasPrefix(subdir, "/") {
		return fmt.Errorf("the directory inside the repository must be relative, got %q", subdir)
	}
	clean := filepath.Clean(filepath.FromSlash(subdir))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%q points outside the repository", subdir)
	}
	return nil
}

// Sync brings the working copy up to date and returns the path to the models.
//
// Clones on the first call and fetches after that. Returns the commit it
// landed on so a caller can report which revision is being served, which is
// the question asked when a number is disputed.
func (s Source) Sync(ctx context.Context) (path string, commit string, err error) {
	if s.Dir == "" {
		return "", "", errors.New("a directory to keep the working copy in is required")
	}
	auth, err := s.auth()
	if err != nil {
		return "", "", err
	}

	repo, err := git.PlainOpen(s.Dir)
	switch {
	case errors.Is(err, git.ErrRepositoryNotExists):
		repo, err = s.clone(ctx, auth)
		if err != nil {
			return "", "", err
		}
	case err != nil:
		return "", "", fmt.Errorf("opening the working copy at %s: %w", s.Dir, err)
	default:
		if err := s.fetch(ctx, repo, auth); err != nil {
			return "", "", err
		}
	}

	head, err := s.checkout(repo)
	if err != nil {
		return "", "", err
	}

	models := s.Dir
	if s.Subdir != "" {
		models = filepath.Join(s.Dir, filepath.FromSlash(s.Subdir))
		// Checked again after joining, rather than trusting the check in
		// Parse. A symlink committed into the repository can point anywhere,
		// and this is the point at which that would matter.
		if err := within(s.Dir, models); err != nil {
			return "", "", err
		}
		if _, err := os.Stat(models); err != nil {
			return "", "", fmt.Errorf(
				"the repository has no %s directory at %s", s.Subdir, head[:min(7, len(head))])
		}
	}
	return models, head, nil
}

// within refuses a resolved path that escaped the repository, following
// symlinks. A repository is somebody else's content even when it is your own.
func within(root, path string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolving the working copy: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		// Absent is reported by the caller with a better message than this
		// one could give.
		return nil
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("the models directory resolves outside the repository")
	}
	return nil
}

func (s Source) clone(ctx context.Context, auth transport.AuthMethod) (*git.Repository, error) {
	opts := &git.CloneOptions{
		URL:  s.URL,
		Auth: auth,
		// One commit of one branch. The history is not what is being served,
		// and a model repository that has been going for years should not cost
		// a large download every time a pod starts.
		Depth:        1,
		SingleBranch: true,
		Tags:         git.NoTags,
	}
	if s.Ref != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(s.Ref)
	}

	repo, err := git.PlainCloneContext(ctx, s.Dir, false, opts)
	if err != nil && s.Ref != "" {
		// The ref may be a tag or a commit rather than a branch. Retry without
		// naming it and let checkout resolve it, which is cheaper than asking
		// the operator to say which kind of ref they wrote.
		opts.ReferenceName = ""
		opts.Depth = 0
		opts.SingleBranch = false
		opts.Tags = git.AllTags
		_ = os.RemoveAll(s.Dir)
		repo, err = git.PlainCloneContext(ctx, s.Dir, false, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("cloning %s: %w", s.URL, err)
	}
	return repo, nil
}

func (s Source) fetch(ctx context.Context, repo *git.Repository, auth transport.AuthMethod) error {
	err := repo.FetchContext(ctx, &git.FetchOptions{
		Auth:     auth,
		Force:    true,
		Tags:     git.AllTags,
		RefSpecs: []config.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
	})
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return fmt.Errorf("fetching %s: %w", s.URL, err)
}

// checkout moves the working tree onto the requested revision.
//
// Hard, and to the remote's copy rather than the local one. Nothing should
// ever be committed here, so a local difference is corruption or somebody
// editing a cache by hand, and in both cases the remote is right.
func (s Source) checkout(repo *git.Repository) (string, error) {
	tree, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("reading the working copy: %w", err)
	}

	hash, err := s.resolve(repo)
	if err != nil {
		return "", err
	}
	if err := tree.Checkout(&git.CheckoutOptions{Hash: hash, Force: true}); err != nil {
		return "", fmt.Errorf("checking out %s: %w", hash, err)
	}
	return hash.String(), nil
}

// resolve turns whatever the operator wrote into a commit.
//
// Tried in order: the remote branch, a tag, a raw revision. A local branch is
// deliberately not consulted, because it lags the remote by exactly one fetch
// and serving yesterday's model while reporting today's commit is the worst
// available outcome.
func (s Source) resolve(repo *git.Repository) (plumbing.Hash, error) {
	if s.Ref == "" {
		return defaultRemoteRef(repo)
	}

	for _, name := range []plumbing.ReferenceName{
		plumbing.NewRemoteReferenceName("origin", s.Ref),
		plumbing.NewTagReferenceName(s.Ref),
	} {
		if ref, err := repo.Reference(name, true); err == nil {
			return ref.Hash(), nil
		}
	}
	if hash, err := repo.ResolveRevision(plumbing.Revision(s.Ref)); err == nil {
		return *hash, nil
	}
	return plumbing.ZeroHash, fmt.Errorf(
		"%q is not a branch, tag or commit in %s", s.Ref, s.URL)
}

// defaultRemoteRef finds the commit to serve when no ref was named.
//
// Read from the remote's refs, never from HEAD. Checking out by hash leaves
// the working copy detached, so on the second sync HEAD is no longer a branch
// and reading it yields the commit already checked out. That is the worst
// possible failure here: the engine keeps serving yesterday's model and
// reports yesterday's commit, so nothing anywhere says a deploy did not land.
func defaultRemoteRef(repo *git.Repository) (plumbing.Hash, error) {
	// origin/HEAD names the remote's default branch. Followed rather than
	// read, because when it is stored as a symbolic reference its own hash is
	// whatever it was at clone time and never updates.
	if ref, err := repo.Reference(plumbing.NewRemoteHEADReferenceName("origin"), false); err == nil {
		if ref.Type() == plumbing.SymbolicReference {
			if target, err := repo.Reference(ref.Target(), true); err == nil {
				return target.Hash(), nil
			}
		}
	}

	iter, err := repo.References()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("reading the repository's refs: %w", err)
	}
	defer iter.Close()

	var branches []*plumbing.Reference
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsRemote() && !strings.HasSuffix(ref.Name().String(), "/HEAD") {
			branches = append(branches, ref)
		}
		return nil
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("reading the repository's refs: %w", err)
	}

	switch len(branches) {
	case 1:
		// The ordinary case: cloned with SingleBranch, so there is exactly one.
		return branches[0].Hash(), nil
	case 0:
		// A clone that has not fetched yet has refs/heads and origin/HEAD but
		// no origin/<branch>. Local HEAD is exactly what was just cloned, so
		// it is right here and only here. Every later sync fetches first and
		// takes one of the branches above, which is why the stale-HEAD trap
		// this function exists to avoid cannot reopen through this line.
		head, err := repo.Head()
		if err != nil {
			return plumbing.ZeroHash, errors.New("the repository has no branches to serve")
		}
		return head.Hash(), nil
	default:
		names := make([]string, 0, len(branches))
		for _, b := range branches {
			names = append(names, b.Name().Short())
		}
		sort.Strings(names)
		return plumbing.ZeroHash, fmt.Errorf(
			"the repository has several branches and does not say which is the default: %s. "+
				"Name one, for example #%s", strings.Join(names, ", "), names[0])
	}
}

// auth reads the credential, if one was named.
//
// The variable is named and only then read, so the token is never an argument.
// A public repository needs none, which is why an unset variable with no name
// given is not an error.
func (s Source) auth() (transport.AuthMethod, error) {
	if s.TokenEnv == "" {
		return nil, nil
	}
	token := strings.TrimSpace(os.Getenv(s.TokenEnv))
	if token == "" {
		return nil, fmt.Errorf(
			"-git-token-env names %s, which is not set. Export a token with read access "+
				"to the repository in it, or drop the flag for a public repository", s.TokenEnv)
	}
	// The username is ignored by GitHub, GitLab and Bitbucket for token auth,
	// but basic auth requires one to be present. "git" is the conventional
	// placeholder and says plainly that it is not a person.
	return &http.BasicAuth{Username: "git", Password: token}, nil
}
