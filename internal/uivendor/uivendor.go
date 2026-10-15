// Package uivendor serves the Preact, htm and signals ESM files that the daemon's page and the room
// server's /admin page both import: one vendored copy, written by `make ui-vendor`.
package uivendor

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

//go:embed esm
var files embed.FS

// Prefix is where Handler serves the files; a page's import map points there.
const Prefix = "/static/esm/"

// Handler serves the vendored files under Prefix.
func Handler() http.Handler {
	esm, _ := fs.Sub(files, "esm") // cannot fail: "esm" is a valid embedded path
	return http.StripPrefix(Prefix, http.FileServerFS(esm))
}

var importMap = regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`)

// CSP is the policy of a page whose only inline script is its import map: the map is allowed by its
// hash, every other script must come from the server itself. It panics on a page with no map, a
// build-time mistake. scriptSrc adds sources to script-src.
func CSP(page []byte, formAction string, scriptSrc ...string) string {
	m := importMap.FindSubmatch(page)
	if m == nil {
		panic("page has no import map")
	}
	sum := sha256.Sum256(m[1])
	return "default-src 'self'; script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'" +
		strings.Join(append([]string{""}, scriptSrc...), " ") + "; base-uri 'none'; frame-ancestors 'none'; form-action " + formAction
}
