// Package webui serves the built single-page application.
//
// The UI is served by the control API itself rather than from separate hosting.
// One origin means one Cloudflare Access boundary: a person who can load the
// page is already authenticated, the API calls carry the same session, and
// there is no CORS surface and no second thing to protect. A static host would
// have needed its own Access application and its own answer for how it talks to
// an API that requires an identity.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var assets embed.FS

// Handler serves the SPA, or nil when no build is embedded.
//
// Returning nil rather than failing is deliberate: the API must still start and
// serve /v1 when the UI has not been built, because an operator debugging an
// incident should not be blocked by a missing frontend bundle.
func Handler() http.Handler {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(dist, "index.html"); err != nil {
		return nil
	}

	files := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The SPA owns its routes, so an unknown path must return index.html
		// and let the client router decide — otherwise a deep link like
		// /projects/nged 404s on refresh even though the app can render it.
		//
		// Paths that look like assets are excluded: serving index.html for a
		// missing .js file turns a broken build into a blank page with no
		// console error, which is far harder to diagnose than a 404.
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(dist, path); err != nil && !strings.Contains(path, ".") {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}

		// The bundle is content-hashed by the build, so it may be cached hard;
		// index.html must not be, or a deploy leaves clients on the old app
		// indefinitely.
		if strings.HasSuffix(r.URL.Path, "/") || strings.HasSuffix(r.URL.Path, "index.html") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
