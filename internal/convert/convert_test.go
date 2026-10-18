package convert

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 001-AC15: the content decides, never the extension alone.
func TestDetect(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	cases := []struct {
		name, file string
		data       []byte
		format     string // empty: ErrUnsupported
		reader     string
	}{
		{"markdown", "plan.md", []byte("# Plan\n\nText."), FormatMD, ""},
		{"markdown upper ext", "PLAN.MARKDOWN", []byte("# Plan"), FormatMD, ""},
		{"markdown starting with html", "plan.md", []byte("<p>hi</p>\n# Plan"), FormatMD, ""},
		{"html", "page.html", []byte("<!doctype html><html><body>hi</body></html>"), FormatHTML, ""},
		{"htm", "page.htm", []byte("<p>hi</p>"), FormatHTML, ""},
		{"pdf", "spec.pdf", []byte("%PDF-1.7\n%..."), FormatPDF, ""},
		{"pdf named md", "spec.md", []byte("%PDF-1.7\n%..."), FormatPDF, ""},
		{"png", "a.png", png, FormatImage, ""},
		{"png named txt", "a.txt", png, FormatImage, ""},
		{"jpeg", "a.jpg", []byte("\xff\xd8\xff\xe0\x00\x10JFIF"), FormatImage, ""},
		{"gif", "a.gif", []byte("GIF89a......"), FormatImage, ""},
		{"webp", "a.webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), FormatImage, ""},
		{"svg", "d.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), FormatImage, ""},
		{"svg with xml decl", "d.svg", []byte(`<?xml version="1.0"?><svg></svg>`), FormatImage, ""},
		{"docx", "f.docx", fixture(t, "fixture.docx"), FormatMD, "docx"},
		{"odt", "f.odt", fixture(t, "fixture.odt"), FormatMD, "odt"},
		{"pptx", "f.pptx", fixture(t, "fixture.pptx"), FormatMD, "pptx"},
		{"docx renamed", "f.bin", fixture(t, "fixture.docx"), FormatMD, "docx"},

		{"plain text", "notes.txt", []byte("hello"), "", ""},
		{"no extension", "notes", []byte("# Plan"), "", ""},
		{"binary named md", "evil.md", []byte("\x00\x01\x02\x03binary"), "", ""},
		{"invalid utf8 named md", "latin1.md", []byte("caf\xe9 au lait"), "", ""},
		// The sniff reads 512 bytes: a NUL past them still makes no text.
		{"NUL past the sniff named md", "a.md", append([]byte(strings.Repeat("# a\n", 200)), 0, 'b'), "", ""},
		{"NUL past the sniff named html", "a.html", append([]byte("<p>"+strings.Repeat("a", 600)), 0), "", ""},
		{"svg ext without svg", "d.svg", []byte("just text"), "", ""},
		{"executable", "run.md", []byte("\x7fELF\x02\x01\x01\x00\x00\x00"), "", ""},
		{"plain zip", "a.docx", zipOf(t, map[string]string{"readme.txt": "x"}), "", ""},
		{"xlsx", "a.xlsx", zipOf(t, map[string]string{"[Content_Types].xml": "x", "xl/workbook.xml": "x"}), "", ""},
		{"ods", "a.ods", zipOf(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.spreadsheet"}), "", ""},
		{"truncated zip", "a.docx", []byte("PK\x03\x04garbage"), "", ""},
		{"gzip", "a.md", []byte("\x1f\x8b\x08\x00\x00\x00\x00\x00"), "", ""},
		{"mp3", "a.md", []byte("ID3\x03\x00\x00\x00\x00\x00\x00"), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Detect(tc.file, tc.data)
			if tc.format == "" {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("got %+v, %v; want ErrUnsupported", got, err)
				}
				return
			}
			if err != nil || got.Format != tc.format || got.Reader != tc.reader || got.ContentType == "" {
				t.Fatalf("got %+v, %v; want format %s reader %q", got, err, tc.format, tc.reader)
			}
		})
	}
}

func TestErrUnsupportedNamesAcceptedTypes(t *testing.T) {
	for _, want := range []string{"Markdown", "HTML", "PDF", "PNG", "DOCX", "ODT", "PPTX"} {
		if !strings.Contains(ErrUnsupported.Error(), want) {
			t.Errorf("message %q does not name %s", ErrUnsupported, want)
		}
	}
}

// pandocPath returns the pandoc to test with. GRILL_ROOM_TEST_PANDOC_REQUIRED=1 (set by the Docker
// build, which tests the pandoc it ships) turns a missing binary or reader into a failure.
func pandocPath(t *testing.T, reader string) string {
	t.Helper()
	required := os.Getenv("GRILL_ROOM_TEST_PANDOC_REQUIRED") == "1"
	skip := t.Skipf
	if required {
		skip = t.Fatalf
	}
	path, err := exec.LookPath(cmp.Or(os.Getenv("GRILL_ROOM_PANDOC"), "pandoc"))
	if err != nil {
		skip("pandoc not found: %v", err)
	}
	out, err := exec.CommandContext(t.Context(), path, "--list-input-formats").Output()
	if err != nil || !strings.Contains("\n"+string(out), "\n"+reader+"\n") {
		skip("%s cannot read %s (pptx needs pandoc 3.8+)", path, reader)
	}
	return path
}

// 001-AC13: each office fixture becomes Markdown. The fixtures are testdata/fixture.md converted
// by the pandoc release the image ships (make fixtures).
func TestPandocFixtures(t *testing.T) {
	for _, reader := range []string{"docx", "odt", "pptx"} {
		t.Run(reader, func(t *testing.T) {
			data := fixture(t, "fixture."+reader)
			typ, err := Detect("fixture."+reader, data)
			if err != nil || typ.Reader != reader {
				t.Fatalf("detect: %+v, %v", typ, err)
			}
			p := NewPandoc(pandocPath(t, reader), 30*time.Second)
			md, err := p.ToMarkdown(t.Context(), typ.Reader, data)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Grill plan", "rollback", "Freeze the schema", "Ship behind a flag", "| Data loss | Alice |"} {
				if !strings.Contains(md, want) {
					t.Errorf("markdown lacks %q:\n%s", want, md)
				}
			}
		})
	}
}

func TestPandocFailures(t *testing.T) {
	path := pandocPath(t, "docx")
	t.Run("corrupt input", func(t *testing.T) {
		_, err := NewPandoc(path, 30*time.Second).ToMarkdown(t.Context(), "docx", []byte("not a zip"))
		if err == nil || !strings.Contains(err.Error(), "pandoc docx") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		start := time.Now()
		_, err := NewPandoc(path, time.Nanosecond).ToMarkdown(t.Context(), "docx", fixture(t, "fixture.docx"))
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
			t.Fatalf("got %v after %s", err, time.Since(start))
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		_, err := NewPandoc(filepath.Join(t.TempDir(), "nope"), time.Second).ToMarkdown(t.Context(), "docx", nil)
		if err == nil {
			t.Fatal("no error")
		}
	})
}

// pandoc sees none of the server's secrets, such as the database URL, only the locale and paths.
func TestPandocEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake pandoc is a shell script")
	}
	t.Setenv("GRILL_ROOM_DATABASE_URL", "postgres://u:secret@127.0.0.1/db")
	t.Setenv("GRILL_ROOM_ADMIN_TOKEN", "secret")
	t.Setenv("LANG", "C.UTF-8")
	fake := filepath.Join(t.TempDir(), "pandoc")
	// A shell builtin: the run without PATH must still list the environment.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexport -p\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	run := func() string {
		t.Helper()
		out, err := NewPandoc(fake, 30*time.Second).ToMarkdown(t.Context(), "docx", nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "GRILL_ROOM_") || strings.Contains(out, "secret") {
			t.Fatalf("pandoc's environment:\n%s", out)
		}
		return out
	}
	if out := run(); !strings.Contains(out, "LANG=") || !strings.Contains(out, "C.UTF-8") {
		t.Fatalf("pandoc lost LANG:\n%s", out)
	}
	// None of the variables it keeps is set: it still gets none of the others.
	for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	run()
}

// 001-AC20: attachments are raster images only; SVG can carry script.
func TestImage(t *testing.T) {
	cases := map[string]struct {
		data []byte
		want string
		ok   bool
	}{
		"png":   {[]byte("\x89PNG\r\n\x1a\n...."), "image/png", true},
		"jpeg":  {[]byte("\xff\xd8\xff\xe0...."), "image/jpeg", true},
		"gif":   {[]byte("GIF89a...."), "image/gif", true},
		"webp":  {[]byte("RIFF\x00\x00\x00\x00WEBPVP"), "image/webp", true},
		"svg":   {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "", false},
		"pdf":   {[]byte("%PDF-1.7"), "", false},
		"empty": {nil, "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ct, ok := Image(tc.data)
			if ok != tc.ok || (ok && ct != tc.want) {
				t.Fatalf("Image() = %q, %v; want %q, %v", ct, ok, tc.want, tc.ok)
			}
		})
	}
}
