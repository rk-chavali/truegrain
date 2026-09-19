package version

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// TestGetAlwaysIdentifiesTheBuild.
//
// Every field here ends up in a support request or an incident timeline. A
// version that reports nothing is the same as having no version at all.
func TestGetAlwaysIdentifiesTheBuild(t *testing.T) {
	info := Get()

	if info.Version == "" {
		t.Error("Version must never be empty; an unstamped build reports dev")
	}
	if info.Go != runtime.Version() {
		t.Errorf("want the running toolchain %q, got %q", runtime.Version(), info.Go)
	}
	if info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Errorf("want %s/%s, got %s/%s", runtime.GOOS, runtime.GOARCH, info.OS, info.Arch)
	}
}

// TestUnstampedBuildReportsDev rather than an empty string, which would render
// as "truegrain  windows/amd64" and tell a reader nothing.
func TestUnstampedBuildReportsDev(t *testing.T) {
	// The package variables are empty under `go test`, since no linker flags
	// are passed, which is exactly the unstamped case.
	if version != "" {
		t.Skip("this build was stamped, so the fallback cannot be observed here")
	}
	if got := Get().Version; got == "" {
		t.Error("an unstamped build must still report a version")
	}
}

func TestStringIsOneReadableLine(t *testing.T) {
	line := Info{
		Version: "v0.1.0",
		Commit:  "4bd515ec0291f19d54e7fed34a2028421c3a8f85",
		Go:      "go1.25.6",
		OS:      "linux",
		Arch:    "arm64",
	}.String()

	if strings.Contains(line, "\n") {
		t.Errorf("the version line must be one line, got:\n%s", line)
	}
	for _, want := range []string{"truegrain", "v0.1.0", "linux/arm64", "go1.25.6"} {
		if !strings.Contains(line, want) {
			t.Errorf("the version line must mention %q, got: %s", want, line)
		}
	}
	// Abbreviated, because a full hash pushes the useful part off a terminal.
	if strings.Contains(line, "4bd515ec0291f19d") {
		t.Errorf("the commit should be abbreviated, got: %s", line)
	}
	if !strings.Contains(line, "4bd515ec0291") {
		t.Errorf("the abbreviated commit is missing, got: %s", line)
	}
}

// TestDirtyIsSaidOutLoud.
//
// A binary built from a tree with uncommitted changes does not correspond to
// any commit, so reporting the commit alone would overstate what is known
// about it. A released artifact must never be dirty.
func TestDirtyIsSaidOutLoud(t *testing.T) {
	line := Info{Version: "v0.1.0", Commit: "abc123def456", Modified: true, OS: "linux", Arch: "amd64"}.String()
	if !strings.Contains(line, "dirty") {
		t.Errorf("a build from a modified tree must say so, got: %s", line)
	}

	clean := Info{Version: "v0.1.0", Commit: "abc123def456", OS: "linux", Arch: "amd64"}.String()
	if strings.Contains(clean, "dirty") {
		t.Errorf("a clean build must not claim to be dirty, got: %s", clean)
	}
}

func TestShortCommitIsNotTruncatedFurther(t *testing.T) {
	// An abbreviated hash that arrives already short must not be sliced again.
	line := Info{Version: "dev", Commit: "abc1234", OS: "linux", Arch: "amd64"}.String()
	if !strings.Contains(line, "abc1234") {
		t.Errorf("a short commit must survive intact, got: %s", line)
	}
}

// TestJSONIsStableForScripts. `truegrain version -json` is what a deployment
// check parses, so the field names are part of the interface.
func TestJSONIsStableForScripts(t *testing.T) {
	raw, err := json.Marshal(Info{Version: "v0.1.0", Commit: "abc", Go: "go1.25.6", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"version", "commit", "go", "os", "arch"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("the JSON form must carry %q, got %s", field, raw)
		}
	}
	// Absent rather than false, so a clean build's output stays quiet.
	if _, present := decoded["modified"]; present {
		t.Errorf("modified must be omitted when false, got %s", raw)
	}
}
