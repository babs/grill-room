package web

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/voices"
)

// 013-AC31: only the server admin changes the lexicon; each change is audited and reaches the pod's
// stream for its next line.
func TestLexicon(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	alice := e.device(t)
	alice.createRoom(t, "Mine", "Alice")
	tok := alice.newToken(t, "full", 30).Token
	gh := voices.Term{Term: "Grafana", Style: "en", FrEn: "gra-fa-na"}
	for _, c := range []struct {
		tok  string
		want int
	}{{"", http.StatusUnauthorized}, {"wrong", http.StatusUnauthorized}, {tok, http.StatusForbidden}} {
		for _, m := range []string{"GET", "POST"} {
			if code, _ := e.do(t, m, "/api/admin/pronunciations", c.tok, gh); code != c.want {
				t.Errorf("%s with %q: %d, want %d", m, c.tok, code, c.want)
			}
		}
		for _, m := range []string{"PUT", "DELETE"} {
			if code, _ := e.do(t, m, "/api/admin/pronunciations/1", c.tok, gh); code != c.want {
				t.Errorf("%s one with %q: %d, want %d", m, c.tok, code, c.want)
			}
		}
	}

	events, _ := e.openStream(t, "/api/tts/events", ttsToken, "")
	if ev := nextEvent(t, events, "lexicon"); !strings.Contains(ev.data, `{"term":"GitHub","style":"en","fr_en":"guite-heub","fr_fr":"","en":""}`) {
		t.Fatalf("lexicon at connect: %.200s", ev.data)
	}
	code, b := e.do(t, "POST", "/api/admin/pronunciations", e.admin, gh)
	if code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, b)
	}
	p := decodeJSON[store.Pronunciation](t, b)
	if ev := nextEvent(t, events, "lexicon"); !strings.Contains(ev.data, `"term":"Grafana","style":"en","fr_en":"gra-fa-na"`) {
		t.Fatal("the added term did not reach the pod")
	}
	gh.Style = "fr"
	id := "/api/admin/pronunciations/" + strconv.FormatInt(p.ID, 10)
	if code, b := e.do(t, "PUT", id, e.admin, gh); code != http.StatusOK {
		t.Fatalf("edit: %d %s", code, b)
	}
	if ev := nextEvent(t, events, "lexicon"); !strings.Contains(ev.data, `"term":"Grafana","style":"fr"`) {
		t.Fatal("the edit did not reach the pod")
	}
	if code, _ := e.do(t, "DELETE", id, e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if ev := nextEvent(t, events, "lexicon"); strings.Contains(ev.data, "Grafana") {
		t.Fatal("the deletion did not reach the pod")
	}
	if code, _ := e.do(t, "DELETE", id, e.admin, nil); code != http.StatusNotFound {
		t.Errorf("delete again: %d", code)
	}
	for action, n := range map[string]int{ActionLexiconAdd: 1, ActionLexiconEdit: 1, ActionLexiconDelete: 1} {
		log := e.auditLog(t, "?action="+action).Entries
		if len(log) != n || log[0].TargetName != "Grafana" || log[0].Actor != store.ActorSuperAdmin {
			t.Errorf("%s audit: %+v", action, log)
		}
	}
}

// 013-AC31: a duplicate ignoring case, an entry with no form, a bad style or term, and the 1001st
// entry are 400.
func TestLexiconRefusals(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []map[string]string{
		{"term": "github", "style": "en", "en": "x"},
		{"term": "Grafana", "style": "en"},
		{"term": "Grafana", "style": "en", "fr_en": "  "},
		{"term": "Grafana", "style": "de", "en": "x"},
		{"term": "", "style": "en", "en": "x"},
		{"term": strings.Repeat("x", 41), "style": "en", "en": "x"},
		{"term": "two\nlines", "style": "en", "en": "x"},
		{"term": "Grafana", "style": "en", "en": strings.Repeat("x", 81)},
	} {
		if code, b := e.do(t, "POST", "/api/admin/pronunciations", e.admin, bad); code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, code, b)
		}
	}
	// A server without voice keeps a lexicon too: the pod reads it once voice is on.
	if code, b := e.do(t, "POST", "/api/admin/pronunciations", e.admin, voices.Term{Term: "Grafana", Style: "en", En: "x"}); code != http.StatusCreated {
		t.Fatalf("add without voice: %d %s", code, b)
	}
	code, b := e.do(t, "GET", "/api/admin/pronunciations", e.admin, nil)
	list := decodeJSON[struct {
		Entries []store.Pronunciation `json:"entries"`
		Max     int                   `json:"max"`
	}](t, b)
	if code != http.StatusOK || list.Max != store.MaxPronunciations || len(list.Entries) != len(voices.Seed())+1 {
		t.Fatalf("list: %d, %d entries, max %d", code, len(list.Entries), list.Max)
	}
	for i := len(list.Entries); i < store.MaxPronunciations; i++ {
		if _, err := e.st.AddPronunciation(t.Context(), voices.Term{Term: "term" + strconv.Itoa(i), Style: "en", En: "x"}, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if code, b := e.do(t, "POST", "/api/admin/pronunciations", e.admin, voices.Term{Term: "Loki", Style: "en", En: "x"}); code != http.StatusBadRequest || !strings.Contains(string(b), "full") {
		t.Errorf("entry 1001: %d %s", code, b)
	}
}

// 013-AC34: demand counts the rooms with a speaking persona and a human who has the room open, in
// the audio or not, so the pod starts before anyone joins the call; it goes back to 0 once they
// are gone.
func TestTTSDemand(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	e.srv.now = func() time.Time { return time.Unix(0, clock.Load()) }
	e.srv.lease = e.srv.now
	r := newTurnRoom(t, e)
	if n := e.srv.ttsDemand(); n != 0 {
		t.Fatalf("nobody here: %d", n)
	}
	_, closeRoom := e.openStream(t, "/api/rooms/"+r.room+"/events?tab=alice-1", r.alice.p.Token, "")
	if n := e.srv.ttsDemand(); n != 1 {
		t.Fatalf("the room open, nobody in the audio: %d", n)
	}
	// 013-AC15: the bots still join only rooms with a listener.
	if rs := e.srv.ttsRoster(t.Context()); len(rs) != 0 {
		t.Fatalf("roster with nobody in the audio: %+v", rs)
	}
	e.inAudio(t, r.room, r.alice.p, true)
	if n := e.srv.ttsDemand(); n != 1 {
		t.Fatalf("a deafened listener: %d", n)
	}
	closeRoom()
	if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/presence", r.alice.p.Token, map[string]any{"tab": "tab1", "gone": true}); code != http.StatusNoContent {
		t.Fatalf("gone: %d %s", code, b)
	}
	// Past the presence grace, once the stream is released.
	waitFor(t, func() bool { clock.Add(int64(presenceGrace + time.Second)); return e.srv.ttsDemand() == 0 })
	e.inAudio(t, r.room, r.alice.p, false)
	if n := e.srv.ttsDemand(); n != 1 {
		t.Fatalf("a listener: %d", n)
	}
	if rs := e.srv.ttsRoster(t.Context()); len(rs) != 1 || rs[0].Room != r.room {
		t.Fatalf("roster with a listener: %+v", rs)
	}
	if code, b := e.do(t, "PATCH", "/api/rooms/"+r.room+"/personas/"+strconv.FormatInt(r.grill.ID, 10), r.alice.p.Token, map[string]bool{"speaks": false}); code != http.StatusOK {
		t.Fatalf("speaks off: %d %s", code, b)
	}
	if n := e.srv.ttsDemand(); n != 0 {
		t.Fatalf("no persona speaks: %d", n)
	}
}
