package gitsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Reading a model out of git.
//
// Two halves worth different kinds of test. Parse is where an operator's
// mistake is caught and where a credential could leak, so it is tested
// directly and exhaustively. Sync talks to a repository, so it is tested
// against a real one built in a temp directory rather than against a mock
// that would agree with whatever this file believes about git.

func TestParseReadsTheShape(t *testing.T) {
	for _, c := range []struct {
		in     string
		url    string
		ref    string
		subdir string
	}{
		{in: "git+https://github.com/acme/models.git", url: "https://github.com/acme/models.git"},
		{in: "git+https://github.com/acme/models.git#main", url: "https://github.com/acme/models.git", ref: "main"},
		{in: "git+https://github.com/acme/models.git#main:models", url: "https://github.com/acme/models.git", ref: "main", subdir: "models"},
		{in: "git+https://github.com/acme/models.git#:models", url: "https://github.com/acme/models.git", subdir: "models"},
		{in: "git+ssh://git@github.com/acme/models.git#v1.2.0", url: "ssh://git@github.com/acme/models.git", ref: "v1.2.0"},
	} {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got.URL != c.url || got.Ref != c.ref || got.Subdir != c.subdir {
			t.Errorf("%s parsed as url=%q ref=%q subdir=%q, want %q %q %q",
				c.in, got.URL, got.Ref, got.Subdir, c.url, c.ref, c.subdir)
		}
	}
}

// TestACredentialInTheURLIsRefused.
//
// A token in a URL reaches every log line, every error and the process list.
// Refused rather than scrubbed, because scrubbing works until one path
// forgets, and the path that forgets is the one in the error handler nobody
// exercises.
func TestACredentialInTheURLIsRefused(t *testing.T) {
	for _, in := range []string{
		"git+https://token@github.com/acme/models.git",
		"git+https://user:hunter2@github.com/acme/models.git",
		"git+https://x-access-token:ghp_abcdef@github.com/acme/models.git",
	} {
		_, err := Parse(in)
		if err == nil {
			t.Errorf("%s was accepted with a credential in it", in)
			continue
		}
		// The refusal must not echo the secret it is refusing.
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "ghp_abcdef") {
			t.Errorf("the refusal repeated the credential: %v", err)
		}
	}
}

// TestOnlyHTTPSAndSSHAreRead.
//
// file:// would let a -models value read anything this process can reach, and
// plain git:// carries no authentication and no encryption, so the model that
// arrives is whatever the network handed over.
func TestOnlyHTTPSAndSSHAreRead(t *testing.T) {
	for _, in := range []string{
		"git+file:///etc",
		"git+git://github.com/acme/models.git",
		"git+http://github.com/acme/models.git",
		"git+github.com/acme/models.git",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("%s was accepted", in)
		}
	}
}

func TestSubdirCannotEscape(t *testing.T) {
	for _, in := range []string{
		"git+https://h/r.git#main:../../etc",
		"git+https://h/r.git#main:..",
		"git+https://h/r.git#main:/etc",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("%s was accepted", in)
		}
	}
	// A nested path that stays inside is fine and common.
	if _, err := Parse("git+https://h/r.git#main:teams/sales/models"); err != nil {
		t.Errorf("a legitimate nested directory was refused: %v", err)
	}
}

func TestANonGitValueIsNotMistakenForOne(t *testing.T) {
	if _, err := Parse("./models"); err == nil {
		t.Error("a local path was parsed as a repository")
	}
}

// origin builds a repository with one commit and returns its path.
func origin(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, repo, dir, files, "the first commit")
	return dir
}

func commit(t *testing.T, repo *git.Repository, dir string, files map[string]string, message string) string {
	t.Helper()
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := tree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func TestSyncClonesThenFetches(t *testing.T) {
	remote := origin(t, map[string]string{"models/one.yaml": "first"})
	src := Source{URL: remote, Dir: filepath.Join(t.TempDir(), "copy"), Subdir: "models"}

	path, first, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(path, "one.yaml")); string(body) != "first" {
		t.Errorf("the clone did not bring the file, got %q", body)
	}

	// A second sync with nothing changed must be a no-op that still answers,
	// because the reload loop calls this on every tick.
	_, again, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if again != first {
		t.Errorf("an unchanged repository reported a different commit: %s then %s", first, again)
	}
}

// TestSyncPicksUpANewCommit is the whole feature: somebody merges, and the
// next poll serves it.
func TestSyncPicksUpANewCommit(t *testing.T) {
	remote := origin(t, map[string]string{"models/one.yaml": "first"})
	src := Source{URL: remote, Dir: filepath.Join(t.TempDir(), "copy"), Subdir: "models"}

	if _, _, err := src.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	repo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	want := commit(t, repo, remote, map[string]string{"models/one.yaml": "second"}, "a change")

	path, got, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync after a new commit: %v", err)
	}
	if got != want {
		t.Errorf("landed on %s, want the new commit %s", got, want)
	}
	if body, _ := os.ReadFile(filepath.Join(path, "one.yaml")); string(body) != "second" {
		t.Errorf("the working copy still holds the old file: %q", body)
	}
}

// TestALocalEditIsDiscarded. Nothing should ever be committed in the working
// copy, so a difference is either corruption or somebody editing a cache by
// hand. The remote is right in both cases.
func TestALocalEditIsDiscarded(t *testing.T) {
	remote := origin(t, map[string]string{"models/one.yaml": "first"})
	src := Source{URL: remote, Dir: filepath.Join(t.TempDir(), "copy"), Subdir: "models"}

	path, _, err := src.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "one.yaml"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, _, err = src.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync after a local edit: %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(path, "one.yaml")); string(body) != "first" {
		t.Errorf("a hand edit survived the sync: %q", body)
	}
}

func TestAMissingSubdirSaysSo(t *testing.T) {
	remote := origin(t, map[string]string{"one.yaml": "first"})
	src := Source{URL: remote, Dir: filepath.Join(t.TempDir(), "copy"), Subdir: "models"}

	_, _, err := src.Sync(context.Background())
	if err == nil {
		t.Fatal("a missing directory must be reported")
	}
	if !strings.Contains(err.Error(), "models") {
		t.Errorf("the error should name the directory: %v", err)
	}
}

func TestAnUnknownRefIsRefused(t *testing.T) {
	remote := origin(t, map[string]string{"one.yaml": "first"})
	src := Source{URL: remote, Dir: filepath.Join(t.TempDir(), "copy"), Ref: "no-such-branch"}

	if _, _, err := src.Sync(context.Background()); err == nil {
		t.Fatal("an unknown ref must be refused rather than silently serving the default branch")
	}
}

// TestANamedTokenVariableMustBeSet, because the alternative is connecting
// anonymously to a private repository and reporting "not found", which sends
// somebody to check the URL instead of the credential.
func TestANamedTokenVariableMustBeSet(t *testing.T) {
	src := Source{URL: "https://example.invalid/r.git", Dir: t.TempDir(), TokenEnv: "TRUEGRAIN_TEST_ABSENT_TOKEN"}

	_, _, err := src.Sync(context.Background())
	if err == nil {
		t.Fatal("an unset token variable must be refused")
	}
	if !strings.Contains(err.Error(), "TRUEGRAIN_TEST_ABSENT_TOKEN") {
		t.Errorf("the error should name the variable: %v", err)
	}
}

func TestSyncNeedsSomewhereToPutIt(t *testing.T) {
	if _, _, err := (Source{URL: "https://example.invalid/r.git"}).Sync(context.Background()); err == nil {
		t.Error("syncing with no directory must be refused")
	}
}
