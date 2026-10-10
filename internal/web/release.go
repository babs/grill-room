package web

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.uber.org/zap"
	"golang.org/x/mod/semver"
)

// DaemonTargets are the platforms a release ships a daemon for; Windows runs the linux build in WSL.
var DaemonTargets = []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"}

// DaemonFile is the release file name of the daemon for target.
func DaemonFile(target string) string { return "grill-room-daemon-" + target }

// Release is the daemon release this server hands out (D28), as `make release` writes it: one
// binary per target, its <binary>.sha256 file, and VERSION.
type Release struct {
	Dir     string
	Version string
	// SHA256 maps a target to its binary's hex digest, as published in its .sha256 file.
	SHA256 map[string]string
}

// LoadRelease reads the release in dir. Every target must be there: install.sh and the room page
// offer them all.
func LoadRelease(dir string) (*Release, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("daemon release: %w", err)
	}
	defer func() { _ = root.Close() }()
	b, err := root.ReadFile("VERSION")
	if err != nil {
		return nil, fmt.Errorf("daemon release: %w", err)
	}
	r := &Release{Dir: dir, Version: strings.TrimSpace(string(b)), SHA256: map[string]string{}}
	if !semver.IsValid(r.Version) {
		return nil, fmt.Errorf("daemon release: VERSION %q is not a semver like v1.2.3", r.Version)
	}
	for _, t := range DaemonTargets {
		f := DaemonFile(t)
		b, err := root.ReadFile(f + ".sha256")
		if err != nil {
			return nil, fmt.Errorf("daemon release: %w", err)
		}
		// sha256sum format: "<hex>  <file>".
		sum, name, _ := strings.Cut(strings.TrimSpace(string(b)), "  ")
		if raw, err := hex.DecodeString(sum); err != nil || len(raw) != 32 || name != f {
			return nil, fmt.Errorf("daemon release: %s.sha256 is not a sha256sum line for %s", f, f)
		}
		if _, err := root.Stat(f); err != nil {
			return nil, fmt.Errorf("daemon release: %w", err)
		}
		r.SHA256[t] = sum
	}
	return r, nil
}

// daemonVersion tells daemons and the room page what this server hands out. Public: install.sh
// and a daemon that is not paired yet have no token.
func (s *Server) daemonVersion(w http.ResponseWriter, _ *http.Request) {
	v := map[string]any{"version": "", "sha256": map[string]string{}, "sandbox_image": s.opts.SandboxImage}
	if r := s.opts.Release; r != nil {
		v["version"], v["sha256"] = r.Version, r.SHA256
	}
	writeJSON(w, http.StatusOK, v)
}

// download serves a release binary or its .sha256 file, by exact name only.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	rel := s.opts.Release
	if rel == nil {
		httpError(w, http.StatusNotFound, "this server hands out no daemon release")
		return
	}
	name := r.PathValue("file")
	known := false
	for _, t := range DaemonTargets {
		known = known || name == DaemonFile(t) || name == DaemonFile(t)+".sha256"
	}
	if !known {
		httpError(w, http.StatusNotFound, "no such file")
		return
	}
	f, err := os.OpenInRoot(rel.Dir, name)
	if err != nil {
		s.log.Error("release_file_unreadable", zap.Error(err))
		httpError(w, http.StatusNotFound, "no such file")
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "release file unreadable")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if strings.HasSuffix(name, ".sha256") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	// The names stay the same from one release to the next.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (s *Server) installScript(w http.ResponseWriter, r *http.Request) {
	if s.opts.Release == nil {
		httpError(w, http.StatusNotFound, "this server hands out no daemon release")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, ui, "ui/install.sh")
}
