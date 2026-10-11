package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 002-AC36: the page sets the cap and the approval mode; a bad value is refused and changes
// nothing; the choice survives a restart and wins over the environment's, which apply only while
// the page saved nothing.
func TestAdminSettings(t *testing.T) {
	d := newTestDaemon(t, nil)
	d.o.SettingsPath = filepath.Join(t.TempDir(), "state", "settings.json")
	e := newAdminEnv(t, d)
	tok := e.login(t)
	if code, _ := e.req(t, "POST", "/api/settings", "", `{"max_model":"haiku"}`, nil); code != http.StatusUnauthorized {
		t.Fatalf("settings without a session: %d", code)
	}
	for _, bad := range []string{`{"max_model":"gpt-5"}`, `{"max_effort":"turbo"}`, `{"approve_all":"yes"}`, `not json`} {
		if code, body := e.req(t, "POST", "/api/settings", tok, bad, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, code, body)
		}
	}
	if s := d.Status().Settings; s != (Settings{}) {
		t.Fatalf("a refused change applied: %+v", s)
	}
	if code, body := e.req(t, "POST", "/api/settings", tok, `{"max_model":"claude-sonnet-5-5","max_effort":"high","approve_all":true}`, nil); code != http.StatusNoContent {
		t.Fatalf("settings: %d %s", code, body)
	}
	want := Settings{MaxModel: "sonnet", MaxEffort: "high", ApproveAll: true}
	code, body := e.req(t, "GET", "/api/status", tok, "", nil)
	var st struct {
		Status Status `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil || st.Status.Settings != want {
		t.Fatalf("status: %d %s", code, body)
	}
	env := Settings{MaxModel: "opus"}
	if got, err := LoadSettings(d.o.SettingsPath, env); err != nil || got != want {
		t.Fatalf("after a restart: %+v %v", got, err)
	}
	if got, err := LoadSettings(filepath.Join(t.TempDir(), "none.json"), Settings{MaxModel: "claude-opus-4-8"}); err != nil || got.MaxModel != "opus" {
		t.Fatalf("environment defaults: %+v %v", got, err)
	}
	if _, err := LoadSettings(filepath.Join(t.TempDir(), "none.json"), Settings{MaxEffort: "turbo"}); err == nil {
		t.Fatal("a bad environment effort loaded")
	}
	broken := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(broken, env); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("a corrupt file: %v", err)
	}
	assertPerm(t, d.o.SettingsPath, 0o600)
}
