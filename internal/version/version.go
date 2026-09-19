// Package version reports what this build is.
//
// A binary that cannot say which commit produced it cannot be traced back to
// one, and a compiled SQL statement whose provenance stops at "some version of
// the engine" is exactly the gap this project exists to close. The audit log
// records the model version on every decision; this is the other half.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set by the linker at release time:
//
//	-ldflags "-X github.com/rk-chavali/truegrain/internal/version.version=v0.1.0 ..."
//
// They are deliberately unexported. A caller reads them through [Info], which
// fills in what the linker did not from the build information Go embeds, so a
// `go install` of this module reports a real commit rather than "dev".
var (
	version = ""
	commit  = ""
	date    = ""
)

// Info describes one build.
type Info struct {
	// Version is the release tag, or "dev" for an untagged build.
	Version string `json:"version"`
	// Commit is the revision the build came from. Empty only when the build
	// had no version control information at all.
	Commit string `json:"commit,omitempty"`
	// Date is when it was built, in RFC 3339.
	Date string `json:"date,omitempty"`
	// Modified reports that the working tree had uncommitted changes. A
	// released artifact must never have this set.
	Modified bool `json:"modified,omitempty"`
	// Go is the toolchain version, which matters when a bug turns out to be in
	// the runtime rather than in this code.
	Go string `json:"go"`
	// OS and Arch are the target this binary was built for.
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// Get returns the build information.
//
// Linker values win when present, because a release knows its own tag. What is
// missing falls back to the VCS stamps Go embeds automatically, which is what
// makes `go install` produce something identifiable rather than blank.
func Get() Info {
	info := Info{
		Version: version,
		Commit:  commit,
		Date:    date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}

	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = setting.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = setting.Value
				}
			case "vcs.modified":
				info.Modified = setting.Value == "true"
			}
		}
		// A module installed with `go install module@v1.2.3` carries its
		// version here even though no linker flag was passed.
		if info.Version == "" && build.Main.Version != "" && build.Main.Version != "(devel)" {
			info.Version = build.Main.Version
		}
	}

	if info.Version == "" {
		info.Version = "dev"
	}
	return info
}

// String renders one line, which is what a support request should contain.
func (i Info) String() string {
	out := "truegrain " + i.Version
	if i.Commit != "" {
		short := i.Commit
		if len(short) > 12 {
			short = short[:12]
		}
		out += " (" + short
		if i.Modified {
			// Said out loud: a binary built from a dirty tree does not
			// correspond to any commit, so its provenance is a guess.
			out += ", dirty"
		}
		out += ")"
	}
	return fmt.Sprintf("%s %s/%s, built with %s", out, i.OS, i.Arch, i.Go)
}
