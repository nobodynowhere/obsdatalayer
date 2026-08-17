// Package ui serves the embedded administration single-page application.
//
// The bundle is compiled into the binary, so the gateway ships as one artifact
// with no separate web server or asset directory to deploy. The Vite build
// writes into ./dist, which is embedded below.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Prefix is the path the SPA is mounted under. It matches the Vite `base`
// setting, so asset URLs the bundle emits resolve correctly.
const Prefix = "/ui/"

//go:embed all:dist
var embedded embed.FS

// Handler returns a handler serving the SPA under Prefix.
//
// Hashed build assets are immutable and cached aggressively; index.html is not,
// so a redeployed gateway is picked up on the next load rather than serving a
// stale shell that points at assets which no longer exist. Unknown paths fall
// back to index.html so client-side routes survive a refresh or a deep link.
func Handler() http.Handler {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		// Only reachable if the embed directive itself is wrong.
		panic("ui: cannot open embedded dist: " + err.Error())
	}
	files := http.FileServer(http.FS(dist))

	return http.StripPrefix(strings.TrimSuffix(Prefix, "/"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "/" {
			clean = "index.html"
		}

		if _, err := fs.Stat(dist, clean); err != nil {
			// Not a real file: hand the SPA its shell and let the router decide.
			serveIndex(w, r, dist)
			return
		}

		if clean == "index.html" {
			serveIndex(w, r, dist)
			return
		}

		// Vite fingerprints filenames, so a given URL's contents never change.
		if strings.HasPrefix(clean, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	}))
}

func serveIndex(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	data, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		http.Error(w, "ui not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// The SPA loads only same-origin assets and talks only to this gateway.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write(data)
}

// IsUIPath reports whether a request path belongs to the SPA bundle.
// Used by the admin listener to serve static assets without credentials.
func IsUIPath(p string) bool {
	return p == "/" || p == strings.TrimSuffix(Prefix, "/") || strings.HasPrefix(p, Prefix)
}
