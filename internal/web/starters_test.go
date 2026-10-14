package web

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

type starterResp struct {
	store.Starter
	Manifest *store.StarterManifest `json:"manifest"`
}

// signedIn returns a device holding a profile named name.
func (e *env) signedIn(t *testing.T, name string) *device {
	t.Helper()
	d := e.device(t)
	d.join(t, e.createRoom(t, "Lobby of "+name).ID, name)
	return d
}

func (d *device) starters(t *testing.T) []store.Starter {
	t.Helper()
	code, b := d.do(t, "GET", "/api/profile/starters", nil)
	if code != http.StatusOK {
		t.Fatalf("starters: %d %s", code, b)
	}
	return decodeJSON[[]store.Starter](t, b)
}

// 006-AC1 006-AC2 006-AC3 006-AC5: the library through the API. Anyone duplicates a built-in into
// "Yours", which nobody else sees; its owner shares it with everyone and deletes it; the
// super-admin lists and removes shared starters in /admin; publishing and deleting are audited.
func TestStarterLibraryAPI(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.device(t).do(t, "GET", "/api/profile/starters", nil); code != http.StatusUnauthorized {
		t.Fatalf("without a profile: %d", code)
	}
	alice, bob := e.signedIn(t, "Alice"), e.signedIn(t, "Bob")
	ls := alice.starters(t)
	if len(ls) != 3 || ls[0].Title != "Spec grill" || ls[0].Library != store.LibraryBuiltin || ls[0].Mine {
		t.Fatalf("built-ins: %+v", ls)
	}
	code, b := alice.do(t, "GET", "/api/profile/starters/"+strconv.FormatInt(ls[1].ID, 10), nil)
	if got := decodeJSON[starterResp](t, b); code != http.StatusOK || got.Manifest.Personas[0].Name != "Facilitator" || got.Version != 1 {
		t.Fatalf("preview: %d %s", code, b)
	}
	builtin := "/api/profile/starters/" + strconv.FormatInt(ls[0].ID, 10)
	for _, path := range []string{builtin + "/publish"} {
		if code, b := alice.do(t, "POST", path, nil); code != http.StatusForbidden || !strings.Contains(string(b), "read-only") {
			t.Fatalf("%s: %d %s", path, code, b)
		}
	}
	if code, _ := alice.do(t, "DELETE", builtin, nil); code != http.StatusForbidden {
		t.Fatalf("delete a built-in: %d", code)
	}
	code, b = alice.do(t, "POST", builtin+"/duplicate", nil)
	mine := decodeJSON[store.Starter](t, b)
	if code != http.StatusCreated || mine.Library != store.LibraryPersonal || mine.Author != "Alice" || !mine.Mine {
		t.Fatalf("duplicate: %d %s", code, b)
	}
	path := "/api/profile/starters/" + strconv.FormatInt(mine.ID, 10)
	if len(bob.starters(t)) != 3 {
		t.Fatal("bob sees alice's personal starter")
	}
	if code, _ := bob.do(t, "GET", path, nil); code != http.StatusNotFound {
		t.Fatalf("bob reads it: %d", code)
	}
	if code, _ := bob.do(t, "POST", path+"/publish", nil); code != http.StatusForbidden {
		t.Fatalf("bob publishes it: %d", code)
	}
	if code, _ := alice.do(t, "POST", path+"/publish", nil, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site publish: %d", code)
	}
	if code, b := alice.do(t, "POST", path+"/publish", nil); code != http.StatusNoContent {
		t.Fatalf("publish: %d %s", code, b)
	}
	if ls := bob.starters(t); len(ls) != 4 || ls[3].Library != store.LibraryInstance || ls[3].Author != "Alice" {
		t.Fatalf("bob sees the shared one: %+v", ls)
	}
	if code, _ := bob.do(t, "DELETE", path, nil); code != http.StatusForbidden {
		t.Fatalf("bob deletes it: %d", code)
	}
	code, b = e.do(t, "GET", "/api/admin/starters", e.admin, nil)
	if got := decodeJSON[[]store.Starter](t, b); code != http.StatusOK || len(got) != 1 || got[0].ID != mine.ID || got[0].Versions != 1 {
		t.Fatalf("admin list: %d %s", code, b)
	}
	if code, _ := alice.do(t, "GET", "/api/admin/starters", nil); code != http.StatusUnauthorized {
		t.Fatalf("a profile lists /admin: %d", code)
	}
	if code, _ := e.do(t, "DELETE", "/api/admin/starters/"+strconv.FormatInt(mine.ID, 10), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("admin removes: %d", code)
	}
	if code, _ := e.do(t, "DELETE", "/api/admin/starters/"+strconv.FormatInt(ls[0].ID, 10), e.admin, nil); code != http.StatusForbidden {
		t.Fatalf("admin removes a built-in: %d", code)
	}
	// Its owner deletes a personal one too, without an audit entry: it was never shared.
	code, b = bob.do(t, "POST", builtin+"/duplicate", nil)
	if code != http.StatusCreated {
		t.Fatalf("bob duplicates: %d", code)
	}
	if code, _ := bob.do(t, "DELETE", "/api/profile/starters/"+strconv.FormatInt(decodeJSON[store.Starter](t, b).ID, 10), nil); code != http.StatusNoContent {
		t.Fatalf("bob deletes his own: %d", code)
	}
	log := e.auditLog(t, "?target=Spec+grill").Entries
	if len(log) != 2 || log[0].Action != ActionStarterDelete || log[0].Actor != store.ActorSuperAdmin ||
		log[1].Action != ActionStarterPublish || !strings.HasPrefix(log[1].Actor, ActorProfile) || !strings.Contains(string(log[1].Details), `"version":1`) {
		t.Fatalf("audit: %+v", log)
	}
}
