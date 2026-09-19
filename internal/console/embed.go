// Package console carries the built UI into the binary.
//
// One binary that serves the API and the pages it drives. The alternative,
// a static bundle on a CDN or a second container, means a version skew
// nobody can see: a console built against one API talking to another, with
// the only symptom being a screen that renders half its data. Embedding
// makes the two the same artifact, so they cannot disagree.
//
// dist/ is a build output and nothing in it is committed: `make console`
// and the Docker build write it before the Go build runs, and the
// console source lives in console/ at the repository root.
//
// A plain `go build` therefore embeds an empty directory, and Placeholder
// below is what it serves instead: Go source rather than a file in dist/,
// because a committed placeholder at that path is deleted by the first
// `npm run build` and then reappears as a deletion in everybody's
// `git status`.
package console

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// FS returns the built console, rooted at its index.
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		// The embed directive guarantees dist/ exists, so this is a
		// programming error rather than a runtime condition.
		panic("console: " + err.Error())
	}
	return sub
}

// Built reports whether a real console is embedded.
//
// Read by `serve console` so it can say which one it is starting with. An
// operator who ran a development build should learn that from the startup
// line rather than from opening the page.
func Built() bool {
	f, err := embedded.Open("dist/index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// Placeholder is the page a binary built without the console serves.
//
// Deliberately says what to run. The alternative, a 404 or a blank page,
// reads as a routing bug and sends somebody into the server code looking
// for a problem that is not there.
const Placeholder = `<!doctype html>
<meta charset="utf-8">
<title>truegrain: the console is not built</title>
<style>
  body { font: 16px/1.6 ui-sans-serif, system-ui, sans-serif; max-width: 34rem;
         margin: 4rem auto; padding: 0 1.5rem; color: #16241f; background: #f1f4f0; }
  code { background: #e7ebe5; padding: .15em .4em; border-radius: 3px; }
</style>
<h1>The console is not built</h1>
<p>
  This binary was compiled without the user interface. The API is running and
  every endpoint works; there is simply nothing to render it.
</p>
<p>Build it once:</p>
<p><code>make console</code></p>
<p>or run it with hot reload, where Vite serves the pages and proxies the API
   to this process:</p>
<p><code>make console-dev</code></p>
<p>
  A release image always has the console in it: the container build compiles
  the UI before it compiles the binary.
</p>
`
