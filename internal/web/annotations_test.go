package web

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/store"
)

// 1x1 PNG.
var png, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")

func annPath(room string, doc int64) string {
	return fmt.Sprintf("/api/rooms/%s/documents/%d/annotations", room, doc)
}

func (e *env) uploadDoc(t *testing.T, room, token, name string, data []byte) store.Document {
	t.Helper()
	code, b := e.upload(t, room, token, name, data, nil)
	if code != http.StatusCreated {
		t.Fatalf("upload %s: %d %s", name, code, b)
	}
	return decodeJSON[store.Document](t, b)
}

var quote = map[string]any{"kind": "text", "exact": "rollback", "prefix": "needs a ", "suffix": " step"}

func images(n int, data []byte) []map[string][]byte {
	out := make([]map[string][]byte, n)
	for i := range out {
		out[i] = map[string][]byte{"data": data}
	}
	return out
}

// 001-AC18 001-AC13: a participant annotates a text range of an MD document, the Markdown copy of
// an office file included; every stream hears of it and every participant reads the thread.
func TestCreateAndListAnnotations(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	plan := e.uploadDoc(t, room.ID, alice.Token, "plan.md", []byte("The rollout needs a rollback step."))
	docx := e.uploadDoc(t, room.ID, alice.Token, "plan.docx", fixture(t, "fixture.docx"))
	ch := e.openEvents(t, room.ID, bob.Token, "")
	for range 2 { // the two joins
		nextMessage(t, ch)
	}

	code, b := e.do(t, "POST", annPath(room.ID, plan.ID), alice.Token, map[string]any{"revision": 1, "anchor": quote, "body": "How long does it take?"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	a := decodeJSON[store.Annotation](t, b)
	if a.Anchor.Exact != "rollback" || a.Messages[0].Author != "Alice" || a.Messages[0].By != store.ByParticipant {
		t.Fatalf("created %+v", a)
	}
	if ev := next(t, ch); ev.event != "annotations" {
		t.Fatalf("got %+v, want an annotations event", ev)
	}

	conv := map[string]any{"kind": "text", "exact": "Converted from docx"}
	if code, b := e.do(t, "POST", annPath(room.ID, docx.ID), bob.Token, map[string]any{"revision": 1, "anchor": conv, "body": "ok"}); code != http.StatusCreated {
		t.Fatalf("office copy: %d %s", code, b)
	}
	code, b = e.do(t, "GET", annPath(room.ID, plan.ID), bob.Token, nil)
	list := decodeJSON[[]store.Annotation](t, b)
	if code != http.StatusOK || len(list) != 1 || list[0].ID != a.ID || list[0].Messages[0].Body != "How long does it take?" {
		t.Fatalf("bob lists %d: %s", code, b)
	}

	// The frame script rides along with every text view.
	if _, body := e.get(t, docPath(room.ID, plan.ID, "/view"), alice.Token); !bytes.Contains(body, []byte("/static/doc/annotate.js")) {
		t.Fatal("markdown view lacks the annotation script")
	}
}

// Every request field is untrusted: anchors, sizes and image types are checked server-side.
func TestAnnotationValidation(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	plan := e.uploadDoc(t, room.ID, alice.Token, "plan.md", []byte("The rollout needs a rollback step."))
	shot := e.uploadDoc(t, room.ID, alice.Token, "shot.png", png)
	other, _ := e.member(t)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)

	cases := map[string]struct {
		path string
		body any
		want int
	}{
		"whole-document comment":   {annPath(room.ID, plan.ID), map[string]any{"body": "overall ok"}, http.StatusCreated},
		"comment on an image":      {annPath(room.ID, shot.ID), map[string]any{"body": "blurry"}, http.StatusCreated},
		"image alone":              {annPath(room.ID, plan.ID), map[string]any{"attachments": images(1, png)}, http.StatusCreated},
		"four images":              {annPath(room.ID, plan.ID), map[string]any{"body": "x", "attachments": images(4, png)}, http.StatusCreated},
		"element anchor":           {annPath(room.ID, plan.ID), map[string]any{"revision": 1, "body": "x", "anchor": map[string]any{"kind": "element", "path": "body > p:nth-of-type(1)", "tag": "p", "fingerprint": "The rollout"}}, http.StatusCreated},
		"anchor on an image":       {annPath(room.ID, shot.ID), map[string]any{"body": "x", "anchor": quote}, http.StatusBadRequest},
		"stale revision":           {annPath(room.ID, plan.ID), map[string]any{"revision": 0, "body": "x", "anchor": quote}, http.StatusConflict},
		"unknown anchor kind":      {annPath(room.ID, plan.ID), map[string]any{"revision": 1, "body": "x", "anchor": map[string]any{"kind": "range"}}, http.StatusBadRequest},
		"exact too long":           {annPath(room.ID, plan.ID), map[string]any{"revision": 1, "body": "x", "anchor": map[string]any{"kind": "text", "exact": strings.Repeat("x", anchor.MaxExact+1)}}, http.StatusBadRequest},
		"path injection":           {annPath(room.ID, plan.ID), map[string]any{"revision": 1, "body": "x", "anchor": map[string]any{"kind": "element", "path": "body > p:nth-of-type(1), *", "tag": "p"}}, http.StatusBadRequest},
		"no body, no image":        {annPath(room.ID, plan.ID), map[string]any{"body": "  "}, http.StatusBadRequest},
		"body too long":            {annPath(room.ID, plan.ID), map[string]any{"body": strings.Repeat("x", maxBodyLen+1)}, http.StatusBadRequest},
		"control character":        {annPath(room.ID, plan.ID), map[string]any{"body": "a\x00b"}, http.StatusBadRequest},
		"five images":              {annPath(room.ID, plan.ID), map[string]any{"body": "x", "attachments": images(5, png)}, http.StatusBadRequest},
		"image over the limit":     {annPath(room.ID, plan.ID), map[string]any{"body": "x", "attachments": images(1, append(append([]byte{}, png...), make([]byte, 1000)...))}, http.StatusRequestEntityTooLarge},
		"svg attachment":           {annPath(room.ID, plan.ID), map[string]any{"body": "x", "attachments": images(1, svg)}, http.StatusUnsupportedMediaType},
		"text as an image":         {annPath(room.ID, plan.ID), map[string]any{"body": "x", "attachments": images(1, []byte("hello"))}, http.StatusUnsupportedMediaType},
		"empty attachment":         {annPath(room.ID, plan.ID), map[string]any{"body": "x", "attachments": images(1, nil)}, http.StatusUnsupportedMediaType},
		"request over the limit":   {annPath(room.ID, plan.ID), map[string]any{"body": strings.Repeat("x", 100<<10)}, http.StatusRequestEntityTooLarge},
		"another room's document":  {annPath(other.ID, plan.ID), map[string]any{"body": "x"}, http.StatusUnauthorized},
		"document of another room": {annPath(room.ID, 9999), map[string]any{"body": "x"}, http.StatusNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if code, b := e.do(t, "POST", tc.path, alice.Token, tc.body); code != tc.want {
				t.Fatalf("got %d %s, want %d", code, b, tc.want)
			}
		})
	}

	t.Run("form content type", func(t *testing.T) {
		code, _ := e.raw(t, "POST", annPath(room.ID, plan.ID), alice.Token, "text/plain", nil, `{"body":"x"}`)
		if code != http.StatusUnsupportedMediaType {
			t.Fatalf("got %d", code)
		}
	})
	t.Run("cross-site, from a sandboxed document", func(t *testing.T) {
		code, _ := e.raw(t, "POST", annPath(room.ID, plan.ID), alice.Token, "application/json", map[string]string{"Sec-Fetch-Site": "cross-site"}, `{"body":"x"}`)
		if code != http.StatusForbidden {
			t.Fatalf("got %d", code)
		}
	})
	t.Run("no token", func(t *testing.T) {
		if code, _ := e.do(t, "POST", annPath(room.ID, plan.ID), "", map[string]any{"body": "x"}); code != http.StatusUnauthorized {
			t.Fatalf("got %d", code)
		}
	})
}

// raw sends body as is, with the given content type and headers.
func (e *env) raw(t *testing.T, method, path, token, ct string, hdr map[string]string, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", ct)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

// 001-AC18 001-AC20: replies with images, status changes, and the image served back inert.
func TestThreadRepliesStatusAndAttachments(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	plan := e.uploadDoc(t, room.ID, alice.Token, "plan.md", []byte("Ship it."))
	_, b := e.do(t, "POST", annPath(room.ID, plan.ID), alice.Token, map[string]any{"body": "overall?"})
	a := decodeJSON[store.Annotation](t, b)
	msgs := fmt.Sprintf("/api/rooms/%s/annotations/%d/messages", room.ID, a.ID)
	status := fmt.Sprintf("/api/rooms/%s/annotations/%d", room.ID, a.ID)

	code, b := e.do(t, "POST", msgs, bob.Token, map[string]any{"body": "see the screenshot", "attachments": images(1, png)})
	if code != http.StatusCreated {
		t.Fatalf("reply: %d %s", code, b)
	}
	m := decodeJSON[store.AnnotationMessage](t, b)
	if m.Author != "Bob" || len(m.Attachments) != 1 || m.Attachments[0].ContentType != "image/png" {
		t.Fatalf("reply %+v", m)
	}

	att := fmt.Sprintf("/api/rooms/%s/attachments/%d", room.ID, m.Attachments[0].ID)
	resp, img := e.get(t, att, alice.Token)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(img, png) || resp.Header.Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(resp.Header.Get("Content-Security-Policy"), "sandbox") || resp.Header.Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("attachment: %d %v", resp.StatusCode, resp.Header)
	}
	other, stranger := e.member(t)
	for path, want := range map[string]int{
		fmt.Sprintf("/api/rooms/%s/attachments/%d", other.ID, m.Attachments[0].ID): http.StatusNotFound,
		fmt.Sprintf("/api/rooms/%s/attachments/x", other.ID):                       http.StatusNotFound,
	} {
		if resp, _ := e.get(t, path, stranger.Token); resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}

	for _, tc := range []struct {
		status string
		want   int
	}{{"done", http.StatusNoContent}, {"open", http.StatusBadRequest}, {"closed", http.StatusBadRequest}, {"reopened", http.StatusNoContent}, {"done", http.StatusNoContent}} {
		if code, b := e.do(t, "PATCH", status, bob.Token, map[string]string{"status": tc.status}); code != tc.want {
			t.Fatalf("status %s: %d %s, want %d", tc.status, code, b, tc.want)
		}
	}
	if code, _ := e.do(t, "POST", msgs, alice.Token, map[string]any{"body": "not quite"}); code != http.StatusCreated {
		t.Fatalf("follow-up: %d", code)
	}
	_, b = e.do(t, "GET", annPath(room.ID, plan.ID), alice.Token, nil)
	if got := decodeJSON[[]store.Annotation](t, b)[0]; got.Status != store.StatusReopened || len(got.Messages) != 3 {
		t.Fatalf("after follow-up: %+v", got)
	}

	// Another room cannot reach this annotation.
	for method, path := range map[string]string{
		"POST":  fmt.Sprintf("/api/rooms/%s/annotations/%d/messages", other.ID, a.ID),
		"PATCH": fmt.Sprintf("/api/rooms/%s/annotations/%d", other.ID, a.ID),
	} {
		body := map[string]any{"body": "x", "status": "done"}
		if code, _ := e.do(t, method, path, stranger.Token, body); code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", method, path, code)
		}
	}
	if code, _ := e.do(t, "PATCH", fmt.Sprintf("/api/rooms/%s/annotations/x", room.ID), alice.Token, map[string]string{"status": "done"}); code != http.StatusNotFound {
		t.Errorf("bad id: %d", code)
	}
}

// Attachments share the room limit with uploads.
func TestAttachmentsFillTheRoom(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	for range 2 {
		e.uploadDoc(t, room.ID, alice.Token, "big.md", md(int(testOptions.MaxUploadBytes)))
	}
	left := testOptions.MaxRoomBytes - 2*testOptions.MaxUploadBytes
	plan := e.uploadDoc(t, room.ID, alice.Token, "plan.md", md(int(left)-len(png)))
	code, b := e.do(t, "POST", annPath(room.ID, plan.ID), alice.Token, map[string]any{"attachments": images(1, png)})
	if code != http.StatusCreated {
		t.Fatalf("last bytes: %d %s", code, b)
	}
	code, b = e.do(t, "POST", annPath(room.ID, plan.ID), alice.Token, map[string]any{"attachments": images(1, png)})
	if code != http.StatusRequestEntityTooLarge || !bytes.Contains(b, []byte("room over its")) {
		t.Fatalf("past the room: %d %s", code, b)
	}
}
