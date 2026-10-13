// Package uikit serves the design tokens and component classes (docs/design.md) that every page of
// the room server and of the daemon links: one file, embedded by both binaries.
package uikit

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed kit
var files embed.FS

// Prefix is where Handler serves the files.
const Prefix = "/static/kit/"

// Handler serves the kit under Prefix.
func Handler() http.Handler {
	kit, _ := fs.Sub(files, "kit") // cannot fail: "kit" is a valid embedded path
	return http.StripPrefix(Prefix, http.FileServerFS(kit))
}
