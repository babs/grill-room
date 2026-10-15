package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/devicestt"
)

const (
	modelsPrefix = "/models/"
	// workerCSP is the policy of the device transcription scripts: a worker's comes from its own
	// response, and onnxruntime-web compiles WebAssembly and starts its threads from /models/.
	workerCSP = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self'; connect-src 'self'; " +
		"base-uri 'none'; frame-ancestors 'none'"
	// sttStaticPrefix holds the room page's transcription workers (010 Phase 1).
	sttStaticPrefix = "/static/stt/"
)

// modelTypes is fixed rather than read from the host's MIME table, which lacks .onnx and .mjs.
var modelTypes = map[string]string{
	".onnx": "application/octet-stream",
	".wasm": "application/wasm",
	".mjs":  "text/javascript; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
}

// deviceSTT is a checked model directory: device mode is offered.
type deviceSTT struct {
	dir   string
	m     devicestt.Manifest
	offer *devicestt.Offer
}

// loadDeviceSTT checks the model directory once, at start: nil when it is unset or any file is
// missing or wrong, which the log names (010-AC5).
func loadDeviceSTT(opts Options, log *zap.Logger) *deviceSTT {
	if opts.DeviceSTTDir == "" {
		return nil
	}
	m := devicestt.Pinned
	if opts.DeviceSTTManifest != nil {
		m = *opts.DeviceSTTManifest
	}
	if err := m.Check(opts.DeviceSTTDir); err != nil {
		log.Warn("device_stt_unavailable", zap.String("dir", opts.DeviceSTTDir), zap.Error(err))
		return nil
	}
	o := m.Offer()
	log.Info("device_stt_offered", zap.String("revision", o.Revision), zap.Int64("bytes", o.Bytes))
	return &deviceSTT{dir: opts.DeviceSTTDir, m: m, offer: o}
}

// serveModel serves GET /models/<revision>/<file>, manifest files only. It runs outside the mux:
// the mux would answer a dotted path with a redirect, where anything but a manifest file is a 404.
func (s *Server) serveModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpError(w, http.StatusMethodNotAllowed, "GET or HEAD only")
		return
	}
	rev, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, modelsPrefix), "/")
	d := s.device
	if d == nil || rev != d.offer.Revision {
		httpError(w, http.StatusNotFound, "no such model file")
		return
	}
	f, ok := d.m.Lookup(name)
	if !ok {
		httpError(w, http.StatusNotFound, "no such model file")
		return
	}
	fh, err := os.Open(filepath.Join(d.dir, f.Name))
	if err != nil {
		s.serverError(w, "model_open_failed", err)
		return
	}
	defer fh.Close() //nolint:errcheck // read only
	st, err := fh.Stat()
	if err != nil {
		s.serverError(w, "model_open_failed", err)
		return
	}
	h := w.Header()
	ctype, ok := modelTypes[filepath.Ext(f.Name)]
	if !ok {
		ctype = "application/octet-stream"
	}
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	http.ServeContent(w, r, "", st.ModTime(), fh)
}
