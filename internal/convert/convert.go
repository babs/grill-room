// Package convert identifies uploaded files by their content and turns office files into Markdown.
package convert

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Formats: how the room shows a document.
const (
	FormatMD    = "md"
	FormatHTML  = "html"
	FormatPDF   = "pdf"
	FormatImage = "image"
)

// Accepted names the upload types, for error messages.
const Accepted = "Markdown (.md), HTML (.html), PDF, PNG, JPEG, GIF, WebP, SVG (.svg), DOCX, ODT, PPTX"

// ErrUnsupported is returned by Detect for a file the room does not accept.
var ErrUnsupported = errors.New("unsupported file type, accepted: " + Accepted)

// Type is what Detect found.
type Type struct {
	// Format is how the room shows the file; office files are shown as their Markdown copy.
	Format string
	// ContentType is the MIME type the original is served with.
	ContentType string
	// Reader is the pandoc input format for office files, empty otherwise.
	Reader string
}

var images = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// Image returns the MIME type of a PNG, JPEG, GIF or WebP image, judged by content. SVG is left
// out: it can carry script.
func Image(data []byte) (string, bool) {
	ct := http.DetectContentType(data)
	return ct, images[ct]
}

// Detect identifies data by its content. The extension only tells apart text formats, which
// content cannot (Markdown, HTML, SVG): a binary file named .md is still rejected.
func Detect(name string, data []byte) (Type, error) {
	ct := http.DetectContentType(data)
	switch {
	case ct == "application/pdf":
		return Type{Format: FormatPDF, ContentType: ct}, nil
	case images[ct]:
		return Type{Format: FormatImage, ContentType: ct}, nil
	case ct == "application/zip":
		return office(data)
	// The store's text columns take no NUL; the sniff above reads the first 512 bytes only.
	case strings.HasPrefix(ct, "text/") && utf8.Valid(data) && bytes.IndexByte(data, 0) < 0:
		switch strings.ToLower(filepath.Ext(name)) {
		case ".md", ".markdown":
			return Type{Format: FormatMD, ContentType: "text/markdown; charset=utf-8"}, nil
		case ".html", ".htm":
			return Type{Format: FormatHTML, ContentType: "text/html; charset=utf-8"}, nil
		case ".svg":
			if bytes.Contains(data, []byte("<svg")) {
				return Type{Format: FormatImage, ContentType: "image/svg+xml"}, nil
			}
		}
	}
	return Type{}, ErrUnsupported
}

const odtMime = "application/vnd.oasis.opendocument.text"

// office tells DOCX, PPTX and ODT apart by the entries every such archive must hold.
func office(data []byte) (Type, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Type{}, ErrUnsupported
	}
	has := map[string]*zip.File{}
	for _, f := range zr.File {
		has[f.Name] = f
	}
	switch {
	case has["[Content_Types].xml"] != nil && has["word/document.xml"] != nil:
		return Type{Format: FormatMD, ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Reader: "docx"}, nil
	case has["[Content_Types].xml"] != nil && has["ppt/presentation.xml"] != nil:
		return Type{Format: FormatMD, ContentType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", Reader: "pptx"}, nil
	case has["mimetype"] != nil && readSmall(has["mimetype"]) == odtMime:
		return Type{Format: FormatMD, ContentType: odtMime, Reader: "odt"}, nil
	}
	return Type{}, ErrUnsupported
}

// readSmall reads at most 128 bytes of f: the declared size of a zip entry cannot be trusted.
func readSmall(f *zip.File) string {
	rc, err := f.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, _ := io.ReadAll(io.LimitReader(rc, 128))
	return strings.TrimSpace(string(b))
}

// Pandoc runs the pandoc binary to turn office files into GitHub Markdown.
type Pandoc struct {
	path    string
	timeout time.Duration
	sem     chan struct{}
}

// pandocHeap caps one conversion's memory: a crafted archive can inflate far past its size.
const pandocHeap = "-M512m"

// NewPandoc runs the binary at path, each conversion bounded by timeout, at most two at a time.
func NewPandoc(path string, timeout time.Duration) *Pandoc {
	return &Pandoc{path: path, timeout: timeout, sem: make(chan struct{}, 2)}
}

// ToMarkdown converts data, read as pandoc format from, to GitHub Markdown.
func (p *Pandoc) ToMarkdown(ctx context.Context, from string, data []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return "", fmt.Errorf("pandoc: waiting for a slot: %w", ctx.Err())
	}
	// No shell: arguments go straight to the binary. --sandbox stops a document from pulling in
	// local files or URLs.
	cmd := exec.CommandContext(ctx, p.path, "+RTS", pandocHeap, "-RTS", "--sandbox", "-f", from, "-t", "gfm") //nolint:gosec // path is operator config, from is one of Detect's constants
	cmd.Stdin = bytes.NewReader(data)
	cmd.Env = pandocEnv()
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	// A killed pandoc must not leave Wait blocked on its pipes.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("pandoc %s: %w", from, ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("pandoc %s: %w: %s", from, err, msg)
	}
	return out.String(), nil
}

// pandocEnv is the little pandoc reads of the environment: the server's own holds the database URL
// and other secrets.
func pandocEnv() []string {
	// Non-nil: a nil Env would hand pandoc the whole environment.
	env := []string{}
	for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
