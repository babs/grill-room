package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
)

// 011-AC10, 011-AC11, 011-AC13: the admin previews then merges; B's browser is then signed into A,
// with the union of rooms; self merges, unknown ids and non-admins are refused.
func TestMergeAPI(t *testing.T) {
	e := newEnv(t)
	shared := e.createRoom(t, "Shared")
	laptop, phone := e.device(t), e.device(t)
	laptop.join(t, e.createRoom(t, "Laptop").ID, "Alice")
	laptop.join(t, shared.ID, "Alice")
	phone.join(t, e.createRoom(t, "Phone").ID, "Ali")
	phone.join(t, shared.ID, "Ali")
	a, b := laptop.profile(t).Profile, phone.profile(t).Profile
	q := fmt.Sprintf("?keep=%d&merge=%d", a.ID, b.ID)

	code, body := e.do(t, "GET", "/api/admin/profiles/merge-preview"+q, e.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("preview: %d %s", code, body)
	}
	pre := decodeJSON[store.MergePreview](t, body)
	both := 0
	for _, r := range pre.Rooms {
		if r.Keep && r.Merge {
			both++
		}
	}
	if pre.Keep.ID != a.ID || pre.Merge.Name != "Ali" || len(pre.Rooms) != 3 || both != 1 {
		t.Fatalf("preview: %s", body)
	}
	for _, c := range []struct {
		path string
		want int
	}{
		{fmt.Sprintf("?keep=%d&merge=%d", a.ID, a.ID), http.StatusBadRequest},
		{fmt.Sprintf("?keep=%d&merge=999999", a.ID), http.StatusNotFound},
		{"?keep=x&merge=1", http.StatusNotFound},
	} {
		if code, b := e.do(t, "GET", "/api/admin/profiles/merge-preview"+c.path, e.admin, nil); code != c.want {
			t.Errorf("preview %s: %d %s, want %d", c.path, code, b, c.want)
		}
	}
	if code, _ := laptop.do(t, "GET", "/api/admin/profiles/merge-preview"+q, nil); code != http.StatusUnauthorized {
		t.Fatalf("preview by a profile: %d", code)
	}
	for _, c := range []struct {
		body map[string]int64
		want int
	}{
		{map[string]int64{"keep": a.ID, "merge": a.ID}, http.StatusBadRequest},
		{map[string]int64{"keep": a.ID, "merge": 999999}, http.StatusNotFound},
	} {
		if code, b := e.do(t, "POST", "/api/admin/profiles/merge", e.admin, c.body); code != c.want || (c.want == http.StatusBadRequest && !strings.Contains(string(b), "cannot be merged into itself")) {
			t.Errorf("merge %v: %d %s, want %d", c.body, code, b, c.want)
		}
	}
	if code, _ := phone.do(t, "POST", "/api/admin/profiles/merge", map[string]int64{"keep": b.ID, "merge": a.ID}); code != http.StatusUnauthorized {
		t.Fatalf("merge by a profile: %d", code)
	}

	code, body = e.do(t, "POST", "/api/admin/profiles/merge", e.admin, map[string]int64{"keep": a.ID, "merge": b.ID})
	if code != http.StatusOK {
		t.Fatalf("merge: %d %s", code, body)
	}
	walEmpty(t, e)
	got := decodeJSON[mergeResp](t, body)
	if got.Keep.ID != a.ID || got.Keep.UUID == "" || got.Moved.Seats != 2 || got.Moved.Sessions != 1 {
		t.Fatalf("merge: %s", body)
	}
	after := phone.profile(t).Profile
	if after == nil || after.ID != a.ID || len(after.Rooms) != 3 {
		t.Fatalf("B's browser after the merge: %+v", after)
	}
	log := e.auditLog(t, "?action="+ActionProfileMerge).Entries
	if len(log) != 1 || log[0].TargetID != strconv.FormatInt(a.ID, 10) || log[0].Actor != store.ActorSuperAdmin ||
		!strings.Contains(string(log[0].Details), `"merge_name":"Ali"`) || !strings.Contains(string(log[0].Details), `"seats":2`) {
		t.Fatalf("audit: %+v", log)
	}
}

// walEmpty fails unless the last change was checkpointed: the WAL keeps no deleted row's pages.
func walEmpty(t *testing.T, e *env) {
	t.Helper()
	if fi, err := os.Stat(e.dbPath + "-wal"); err != nil || fi.Size() != 0 {
		t.Fatalf("WAL after the change: %v %v", fi, err)
	}
}

// ghost joins a fresh room through an invite the admin then revokes: an empty profile.
func (e *env) ghost(t *testing.T) (*device, int64) {
	t.Helper()
	room := e.createRoom(t, "Gone")
	inv := e.createInvite(t, room.ID)
	d := e.device(t)
	d.joinVia(t, inv.Token, "Ghost")
	id := d.profile(t).Profile.ID
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/invites/%d", room.ID, inv.ID), e.admin, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	return d, id
}

// 011-AC16, 011-AC17, 011-AC18: the switch, on by default and audited when changed; off, a run
// deletes nothing; on, the hourly loop deletes on the server's clock, with one system entry, and the
// purged browser is signed out.
func TestPurgeProfilesLoopAndSwitch(t *testing.T) {
	e := newEnv(t)
	_, skew := fixedClock(e)
	d, id := e.ghost(t)
	if code, b := e.do(t, "GET", "/api/admin/settings", e.admin, nil); code != http.StatusOK || !decodeJSON[store.InstanceSettings](t, b).PurgeEmptyProfiles {
		t.Fatalf("default: %d %s", code, b)
	}
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 10, RoomCreation: store.CreationAnyone, PurgeEmptyProfiles: false})
	if log := e.auditLog(t, "?action="+ActionSettings).Entries; len(log) != 1 || !strings.Contains(string(log[0].Details), `"purge_empty_profiles":false`) {
		t.Fatalf("settings audit: %+v", log)
	}
	skew(25 * time.Hour)
	if got, err := e.srv.purgeProfiles(t.Context(), store.EmptyProfileIdle); err != nil || len(got) != 0 {
		t.Fatalf("switched off: %v %v", got, err)
	}
	// A field left out keeps its value.
	if code, b := e.do(t, "PUT", "/api/admin/settings", e.admin, map[string]any{"rooms_per_profile": 10, "storage_quota_bytes": 0, "room_creation": "anyone"}); code != http.StatusOK ||
		decodeJSON[store.InstanceSettings](t, b).PurgeEmptyProfiles {
		t.Fatalf("left out: %d %s", code, b)
	}
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 10, RoomCreation: store.CreationAnyone, PurgeEmptyProfiles: true})

	defer func(d time.Duration) { profilePurgeEvery = d }(profilePurgeEvery)
	profilePurgeEvery = 10 * time.Millisecond
	loopCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.srv.PurgeProfiles(loopCtx); close(done) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) { // polls, bounded by the deadline
		if len(e.auditLog(t, "?action="+ActionProfilePurge).Entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop purged nothing")
		}
	}
	cancel()
	<-done
	log := e.auditLog(t, "?action="+ActionProfilePurge).Entries
	if len(log) != 1 || log[0].Actor != store.ActorSystem || !strings.Contains(string(log[0].Details), `"count":1`) {
		t.Fatalf("purge audit: %+v", log)
	}
	if code, b := e.do(t, "GET", "/api/profiles", e.admin, nil); code != http.StatusOK || strings.Contains(string(b), fmt.Sprintf(`"id":%d,`, id)) {
		t.Fatalf("profile %d still listed: %s", id, b)
	}
	if p := d.profile(t).Profile; p != nil {
		t.Fatalf("the purged browser still holds a profile: %+v", p)
	}
}

// 011-AC17: a PUT changes only the fields it sends, and a refused one changes nothing.
func TestSettingsPatchesKeepOtherFields(t *testing.T) {
	e := newEnv(t)
	for _, patch := range []map[string]any{{"plugin_max_mb": 7}, {"purge_empty_profiles": false}} {
		if code, b := e.do(t, "PUT", "/api/admin/settings", e.admin, patch); code != http.StatusOK {
			t.Fatalf("%v: %d %s", patch, code, b)
		}
	}
	for _, bad := range []any{map[string]any{"plugin_max_mb": 0, "purge_empty_profiles": true}, []int{1}} {
		if code, _ := e.do(t, "PUT", "/api/admin/settings", e.admin, bad); code != http.StatusBadRequest {
			t.Fatalf("%v: %d", bad, code)
		}
	}
	_, b := e.do(t, "GET", "/api/admin/settings", e.admin, nil)
	if v := decodeJSON[store.InstanceSettings](t, b); v.PluginMaxMB != 7 || v.PurgeEmptyProfiles || v.RoomsPerProfile != store.DefaultRoomsPerProfile {
		t.Fatalf("settings: %+v", v)
	}
	if log := e.auditLog(t, "?action="+ActionSettings).Entries; len(log) != 2 || !strings.Contains(string(log[0].Details), `"plugin_max_mb":7`) ||
		!strings.Contains(string(log[0].Details), `"purge_empty_profiles":false`) {
		t.Fatalf("audit: %+v", log)
	}
}

// 011-AC18: a purged profile's browser reads as signed out while email works, so the join page
// offers email recovery.
func TestPurgedBrowserIsSignedOut(t *testing.T) {
	e, _ := newMailEnv(t, time.Minute)
	_, skew := fixedClock(e)
	d, _ := e.ghost(t)
	skew(25 * time.Hour)
	if got, err := e.srv.purgeProfiles(t.Context(), store.EmptyProfileIdle); err != nil || len(got) != 1 {
		t.Fatalf("purge: %v %v", got, err)
	}
	walEmpty(t, e)
	if got := d.profile(t); got.Profile != nil || !got.EmailEnabled {
		t.Fatalf("after the purge: %+v", got)
	}
}

// 002-AC5: deleting a profile or a room checkpoints the WAL, so the deleted rows leave the files.
func TestDeletesCheckpoint(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	d := e.device(t)
	d.join(t, room.ID, "Alice")
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("/api/profiles/%d", d.profile(t).Profile.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("delete profile: %d", code)
	}
	walEmpty(t, e)
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID, e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("delete room: %d", code)
	}
	walEmpty(t, e)
}
