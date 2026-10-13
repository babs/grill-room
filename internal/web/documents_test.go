package web

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
)

// Small limits keep the 413 cases cheap; the defaults (20 MB, 200 MB) are config.
var testOptions = Options{
	// Tests send X-Forwarded-For as if behind a proxy on loopback.
	TrustedProxies:     []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
	MaxUploadBytes:     20000,
	MaxRoomBytes:       50000,
	MaxAttachmentBytes: 1000,
	ToMarkdown: func(_ context.Context, from string, data []byte) (string, error) {
		switch {
		case bytes.Contains(data, []byte("pandoc-missing")):
			return "", fmt.Errorf("pandoc %s: %w", from, exec.ErrNotFound)
		case bytes.Contains(data, []byte("pandoc-fails")):
			return "", errors.New("pandoc " + from + ": exit status 63")
		}
		return "# Converted from " + from + "\n", nil
	},
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../convert/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// upload posts data as the "file" field of a multipart form.
func (e *env) upload(t *testing.T, room, token, name string, data []byte, hdr map[string]string) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("note", "fields before the file are skipped")
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, err := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/api/rooms/"+room+"/documents", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// reply is a response whose body was read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

// get fetches path with a bearer token.
func (e *env) get(t *testing.T, path, token string) (reply, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", e.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header}, b
}

func (e *env) member(t *testing.T) (store.Room, joined) {
	t.Helper()
	room := e.createRoom(t, "Docs")
	return room, e.join(t, e.createInvite(t, room.ID).Token, "Alice")
}

func md(n int) []byte { return bytes.Repeat([]byte("a"), n) }

// 001-AC14: per-file and per-room limits answer 413, the room one naming the room limit.
func TestUploadLimits(t *testing.T) {
	type step struct {
		size     int
		want     int
		contains string
	}
	const over = "file is over the 20000 bytes limit"
	cases := map[string][]step{
		"exactly the file limit":       {{20000, http.StatusCreated, ""}},
		"one byte over the file limit": {{20001, http.StatusRequestEntityTooLarge, over}},
		"far over the file limit":      {{1 << 20, http.StatusRequestEntityTooLarge, over}},
		"fills the room exactly":       {{20000, http.StatusCreated, ""}, {20000, http.StatusCreated, ""}, {10000, http.StatusCreated, ""}},
		"one byte past the room limit": {{20000, http.StatusCreated, ""}, {20000, http.StatusCreated, ""}, {10001, http.StatusRequestEntityTooLarge, "room over its 50000 bytes limit"}},
		"refused upload uses no room":  {{20001, http.StatusRequestEntityTooLarge, over}, {20000, http.StatusCreated, ""}, {20000, http.StatusCreated, ""}, {10000, http.StatusCreated, ""}},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			room, alice := e.member(t)
			for i, st := range steps {
				code, body := e.upload(t, room.ID, alice.Token, "plan.md", md(st.size), nil)
				if code != st.want || !strings.Contains(string(body), st.contains) {
					t.Fatalf("step %d (%d bytes): %d %s, want %d containing %q", i, st.size, code, body, st.want, st.contains)
				}
			}
		})
	}
}

func TestSizeText(t *testing.T) {
	for n, want := range map[int64]string{20 << 20: "20 MB", 200 << 20: "200 MB", 1000: "1000 bytes"} {
		if got := sizeText(n); got != want {
			t.Errorf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
}

// 001-AC11 001-AC12 001-AC13 001-AC15: accepted types by content, 415 naming them otherwise.
func TestUploadTypes(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	cases := []struct {
		name   string
		file   string
		data   []byte
		want   int
		format string
	}{
		{"markdown", "plan.md", []byte("# Plan"), http.StatusCreated, "md"},
		{"html", "page.html", []byte("<p>hi</p>"), http.StatusCreated, "html"},
		{"pdf", "spec.pdf", []byte("%PDF-1.4\n"), http.StatusCreated, "pdf"},
		{"png", "shot.png", png, http.StatusCreated, "image"},
		{"svg", "d.svg", []byte("<svg></svg>"), http.StatusCreated, "image"},
		{"docx", "plan.docx", fixture(t, "fixture.docx"), http.StatusCreated, "md"},
		{"text file", "notes.txt", []byte("hello"), http.StatusUnsupportedMediaType, ""},
		{"binary named md", "evil.md", []byte("\x00\x01\x02binary"), http.StatusUnsupportedMediaType, ""},
		{"zip named docx", "fake.docx", []byte("PK\x03\x04not an office file"), http.StatusUnsupportedMediaType, ""},
		{"elf named pdf", "x.pdf", []byte("\x7fELF\x02\x01\x01"), http.StatusUnsupportedMediaType, ""},
	}
	e := newEnv(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			room, alice := e.member(t)
			code, body := e.upload(t, room.ID, alice.Token, tc.file, tc.data, nil)
			if code != tc.want {
				t.Fatalf("got %d %s, want %d", code, body, tc.want)
			}
			if tc.want == http.StatusUnsupportedMediaType {
				for _, typ := range []string{"Markdown", "HTML", "PDF", "PNG", "DOCX", "ODT", "PPTX"} {
					if !strings.Contains(string(body), typ) {
						t.Errorf("415 message does not name %s: %s", typ, body)
					}
				}
				return
			}
			if d := decodeJSON[store.Document](t, body); d.Format != tc.format || d.Kind != store.DocUpload || d.Author != "Alice" {
				t.Fatalf("document: %+v", d)
			}
		})
	}
}

// A full room answers 413 before pandoc runs: the marker would turn a conversion into a 422.
func TestFullRoomSkipsConversion(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	for range 2 {
		if code, body := e.upload(t, room.ID, alice.Token, "a.md", md(20000), nil); code != http.StatusCreated {
			t.Fatalf("fill: %d %s", code, body)
		}
	}
	docx := append(fixture(t, "fixture.docx"), "pandoc-fails"...)
	if code, body := e.upload(t, room.ID, alice.Token, "x.docx", docx, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d %s, want 413", code, body)
	}
}

func TestUploadSlotsAndReadDeadline(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	for range cap(e.srv.uploads) {
		e.srv.uploads <- struct{}{}
	}
	if code, _ := e.upload(t, room.ID, alice.Token, "a.md", []byte("# a"), nil); code != http.StatusServiceUnavailable {
		t.Fatalf("all slots taken: %d, want 503", code)
	}
	for range cap(e.srv.uploads) {
		<-e.srv.uploads
	}

	old := uploadReadTimeout
	uploadReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { uploadReadTimeout = old })
	// The body announces 1000 bytes and sends 10: the handler must give up, not wait forever.
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", strings.TrimPrefix(e.ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /api/rooms/%s/documents HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n"+
		"Content-Type: multipart/form-data; boundary=b\r\nContent-Length: 1000\r\n\r\n--b\r\nContent-Dis", room.ID, alice.Token)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stalled body: %d, want 400", resp.StatusCode)
	}
}

func TestUploadRequestErrors(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/documents", alice.Token, map[string]string{"file": "x"}); code != http.StatusUnsupportedMediaType {
		t.Errorf("json body: %d, want 415", code)
	}
	if code, _ := e.upload(t, room.ID, alice.Token, "empty.md", nil, nil); code != http.StatusBadRequest {
		t.Errorf("empty file: %d, want 400", code)
	}
	if code, _ := e.upload(t, room.ID, alice.Token, "x.docx", append(fixture(t, "fixture.docx"), "pandoc-fails"...), nil); code != http.StatusUnprocessableEntity {
		t.Errorf("conversion failure: %d, want 422", code)
	}
	if code, _ := e.upload(t, room.ID, alice.Token, "x.docx", append(fixture(t, "fixture.docx"), "pandoc-missing"...), nil); code != http.StatusServiceUnavailable {
		t.Errorf("pandoc missing: %d, want 503", code)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/api/rooms/"+room.ID+"/documents", strings.NewReader("--b\r\n\r\nx\r\n--b--\r\n"))
	req.Header.Set("Authorization", "Bearer "+alice.Token)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no file field: %d, want 400", resp.StatusCode)
	}
	for _, site := range []string{"cross-site", "same-site", "none"} {
		if code, _ := e.upload(t, room.ID, alice.Token, "plan.md", []byte("# x"), map[string]string{"Sec-Fetch-Site": site}); code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site %s: %d, want 403", site, code)
		}
	}
	if code, _ := e.upload(t, room.ID, alice.Token, "plan.md", []byte("# x"), map[string]string{"Sec-Fetch-Site": "same-origin"}); code != http.StatusCreated {
		t.Errorf("same-origin: %d, want 201", code)
	}
	docs := e.documents(t, room.ID, alice.Token)
	if len(docs) != 1 {
		t.Fatalf("only the same-origin upload is stored, got %+v", docs)
	}
}

func (e *env) documents(t *testing.T, room, token string) []store.Document {
	t.Helper()
	resp, body := e.get(t, "/api/rooms/"+room+"/documents", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("documents: %d %s", resp.StatusCode, body)
	}
	return decodeJSON[[]store.Document](t, body)
}

func docPath(room string, id int64, suffix string) string {
	return "/api/rooms/" + room + "/documents/" + strconv.FormatInt(id, 10) + suffix
}

// 001-AC11 001-AC12 001-AC13: each kind is served in its confinement, originals byte for byte.
func TestDocumentViews(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	add := func(name string, data []byte) store.Document {
		code, body := e.upload(t, room.ID, alice.Token, name, data, nil)
		if code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", name, code, body)
		}
		return decodeJSON[store.Document](t, body)
	}
	mdDoc := add("plan.md", []byte("# Plan\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```mermaid\ngraph TD; A-->B\n```\n"))
	htmlDoc := add("page.html", []byte("<!doctype html><title>t</title><script>alert(1)</script>"))
	pdf := []byte("%PDF-1.4\nbody")
	pdfDoc := add("spec.pdf", pdf)
	docx := fixture(t, "fixture.docx")
	docxDoc := add("plan.docx", docx)
	pngDoc := add("shot.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))

	sandboxed := func(t *testing.T, resp reply) {
		t.Helper()
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.HasPrefix(csp, "sandbox") || strings.Contains(csp, "allow-same-origin") || !strings.Contains(csp, "default-src 'none'") {
			t.Fatalf("CSP %q does not confine the document", csp)
		}
	}
	t.Run("markdown renders like GitHub with mermaid loaded", func(t *testing.T) {
		resp, body := e.get(t, docPath(room.ID, mdDoc.ID, "/view"), alice.Token)
		sandboxed(t, resp)
		for _, want := range []string{`<h1 id="plan">Plan</h1>`, "<table>", `<pre class="mermaid">`, "mermaid.min.js", "markdown-body"} {
			if !strings.Contains(string(body), want) {
				t.Errorf("view lacks %s", want)
			}
		}
		if strings.Contains(string(body), "tex-mml-chtml.js") {
			t.Error("MathJax loaded for a document without math")
		}
	})
	// 008-AC7: the first paint at the reader's width; anything else is the default, narrow.
	t.Run("markdown width from the query", func(t *testing.T) {
		for query, want := range map[string]string{
			"":                              `<html lang="en">`,
			"?width=narrow":                 `<html lang="en">`,
			"?width=large":                  `<html lang="en" data-width="large">`,
			"?width=full":                   `<html lang="en" data-width="full">`,
			"?width=full%22%3E%3Cscript%3E": `<html lang="en">`,
			"?width=FULL&width=large":       `<html lang="en">`,
		} {
			_, body := e.get(t, docPath(room.ID, mdDoc.ID, "/view"+query), alice.Token)
			if !strings.Contains(string(body), want) {
				t.Errorf("%q: no %s in %.80s", query, want, body)
			}
		}
		if _, body := e.get(t, docPath(room.ID, htmlDoc.ID, "/view?width=full"), alice.Token); strings.Contains(string(body), "data-width") {
			t.Errorf("an HTML document got a width: %s", body)
		}
	})
	t.Run("html is served as is, sandboxed, with the annotation script", func(t *testing.T) {
		resp, body := e.get(t, docPath(room.ID, htmlDoc.ID, "/view"), alice.Token)
		sandboxed(t, resp)
		if string(body) != "<!doctype html>"+string(annotateScript)+"<title>t</title><script>alert(1)</script>" {
			t.Fatalf("body %q", body)
		}
	})
	t.Run("office shows its markdown copy and keeps the original", func(t *testing.T) {
		resp, body := e.get(t, docPath(room.ID, docxDoc.ID, "/view"), alice.Token)
		sandboxed(t, resp)
		if !strings.Contains(string(body), "Converted from docx") {
			t.Fatalf("view: %s", body)
		}
		resp, body = e.get(t, docPath(room.ID, docxDoc.ID, "/original"), alice.Token)
		if !bytes.Equal(body, docx) || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("original: %d bytes, %q", len(body), resp.Header.Get("Content-Disposition"))
		}
		sandboxed(t, resp)
	})
	t.Run("pdf original inline, not sandboxed for the viewer", func(t *testing.T) {
		resp, body := e.get(t, docPath(room.ID, pdfDoc.ID, "/original"), alice.Token)
		if !bytes.Equal(body, pdf) || resp.Header.Get("Content-Type") != "application/pdf" ||
			resp.Header.Get("Content-Disposition") != `inline; filename=spec.pdf` || resp.Header.Get("Content-Security-Policy") != pdfCSP {
			t.Fatalf("pdf: %q %v", body, resp.Header)
		}
		if resp, _ := e.get(t, docPath(room.ID, pdfDoc.ID, "/view"), alice.Token); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("pdf text view: %d, want 404", resp.StatusCode)
		}
	})
	t.Run("image original inline and sandboxed", func(t *testing.T) {
		resp, _ := e.get(t, docPath(room.ID, pngDoc.ID, "/original"), alice.Token)
		if resp.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
			t.Fatalf("png: %v", resp.Header)
		}
		sandboxed(t, resp)
	})
	t.Run("list and lookups stay in the room", func(t *testing.T) {
		docs := e.documents(t, room.ID, alice.Token)
		if len(docs) != 5 || docs[3].Name != "plan.docx" || docs[3].Revisions != 1 || docs[2].Revisions != 0 {
			t.Fatalf("documents: %+v", docs)
		}
		other, bob := e.member(t)
		for _, p := range []string{"", "/view", "/original"} {
			if resp, _ := e.get(t, docPath(other.ID, mdDoc.ID, p), bob.Token); resp.StatusCode != http.StatusNotFound {
				t.Errorf("another room's document%s: %d, want 404", p, resp.StatusCode)
			}
		}
		if resp, _ := e.get(t, "/api/rooms/"+room.ID+"/documents/x", alice.Token); resp.StatusCode != http.StatusNotFound {
			t.Errorf("bad id: %d", resp.StatusCode)
		}
	})
}

// 001-AC17: an agent document lists its revisions and diffs any two, word by word.
func TestAgentDocumentRevisionsAndDiff(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	ctx := context.Background()
	d, err := e.st.CreateAgentDocument(ctx, room.ID, "synthesis.md", "md", "# Synthesis\n\nThe quick brown fox.\n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"# Synthesis\n\nThe quick red fox.\n", "# Synthesis\n\nThe slow red fox.\n"} {
		if _, err := e.st.AddRevision(ctx, room.ID, d.ID, body, "agent"); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := e.get(t, docPath(room.ID, d.ID, ""), alice.Token)
	got := decodeJSON[documentResp](t, body)
	if resp.StatusCode != http.StatusOK || got.Document.Kind != store.DocAgent || len(got.Revisions) != 3 || got.Revisions[2].N != 3 {
		t.Fatalf("document: %d %+v", resp.StatusCode, got)
	}
	cases := []struct {
		query    string
		want     int
		contains []string
	}{
		{"", http.StatusOK, []string{"The slow red fox."}},
		{"?rev=1", http.StatusOK, []string{"The quick brown fox."}},
		{"?rev=2&from=1", http.StatusOK, []string{`<del class="gg-del">brown </del><ins class="gg-ins">red</ins>`, "github-diff.css"}},
		{"?rev=3&from=1", http.StatusOK, []string{`<del class="gg-del">quick brown </del><ins class="gg-ins">slow red</ins>`}},
		{"?rev=1&from=3", http.StatusOK, []string{`<ins class="gg-ins">quick brown</ins>`}},
		{"?rev=4", http.StatusNotFound, nil},
		{"?rev=2&from=9", http.StatusNotFound, nil},
		{"?rev=two", http.StatusBadRequest, nil},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			resp, body := e.get(t, docPath(room.ID, d.ID, "/view"+tc.query), alice.Token)
			if resp.StatusCode != tc.want {
				t.Fatalf("%d %s", resp.StatusCode, body)
			}
			for _, want := range tc.contains {
				if !strings.Contains(string(body), want) {
					t.Errorf("lacks %s:\n%s", want, body)
				}
			}
		})
	}
	if resp, _ := e.get(t, docPath(room.ID, d.ID, "/original"), alice.Token); resp.StatusCode != http.StatusNotFound {
		t.Errorf("agent original: %d, want 404", resp.StatusCode)
	}
}

// The frame's script and the diff style go into the head, never before the doctype.
func TestInHeadKeepsDoctypeFirst(t *testing.T) {
	const tag = `<script src="/x.js"></script>`
	cases := map[string]struct{ in, want string }{
		"header only":   {"<header>x</header>", tag + "<header>x</header>"},
		"head":          {"<!doctype html><html><HEAD lang=x><title>t</title>", `<!doctype html><html><HEAD lang=x>` + tag + `<title>t</title>`},
		"no head":       {"<p>x</p>", tag + "<p>x</p>"},
		"broken head":   {"<p>x</p><head", tag + "<p>x</p><head"},
		"doctype only":  {"<!DOCTYPE html>\n<p>x</p>", "<!DOCTYPE html>" + tag + "\n<p>x</p>"},
		"comment first": {"<!-- c -->\n<!doctype html><body>", "<!-- c -->\n<!doctype html>" + tag + "<body>"},
		"late doctype":  {"<p>x</p><!doctype html>", tag + "<p>x</p><!doctype html>"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := string(inHead([]byte(tc.in), []byte(tag))); got != tc.want {
				t.Fatalf("got %q", got)
			}
		})
	}
}

// Live updates: an upload or a revision reaches every open stream as a "documents" event.
func TestEventsAnnounceDocuments(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	ch := e.openEvents(t, room.ID, bob.Token, "")
	// No documents event on connect: the client lists documents once the stream is open.
	if m := nextMessage(t, ch); m.Body != "Alice joined" {
		t.Fatalf("got %+v", m)
	}
	if m := nextMessage(t, ch); m.Body != "Bob joined" {
		t.Fatalf("got %+v", m)
	}
	if code, body := e.upload(t, room.ID, alice.Token, "plan.md", []byte("# Plan"), nil); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, body)
	}
	if ev := next(t, ch); ev.event != "documents" {
		t.Fatalf("got %+v, want a documents event", ev)
	}
	d, err := e.st.CreateAgentDocument(context.Background(), room.ID, "s.md", "md", "v1", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddRevision(context.Background(), room.ID, d.ID, "v2", "agent"); err != nil {
		t.Fatal(err)
	}
	e.bus.Notify(room.ID)
	if ev := next(t, ch); ev.event != "documents" {
		t.Fatalf("got %+v, want a documents event", ev)
	}
}
