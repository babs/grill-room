package web

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

func (e *env) rooms(t *testing.T) roomsResp {
	t.Helper()
	code, b := e.do(t, "GET", "/api/admin/rooms", e.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("rooms: %d %s", code, b)
	}
	return decodeJSON[roomsResp](t, b)
}

func roomIn(t *testing.T, r roomsResp, id string) roomOverview {
	t.Helper()
	for _, o := range r.Rooms {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("room %s not listed: %+v", id, r.Rooms)
	return roomOverview{}
}

// 004-AC2: the rooms view lists each room with its counts, its volume as the store holds it, its
// quota and whether its host's daemon is online.
func TestAdminRoomsView(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	quiet := e.createRoom(t, "Quiet")
	e.uploadDoc(t, room.ID, alice.Token, "plan.md", []byte("# Plan ✓"))
	if code, b := e.do(t, "POST", "/api/rooms/"+room.ID+"/messages", alice.Token, map[string]string{"body": "hello"}); code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
	got := e.rooms(t)
	if got.DefaultQuota != testOptions.MaxRoomBytes || len(got.Rooms) != 2 {
		t.Fatalf("rooms: %+v", got)
	}
	want, err := e.st.RoomOverviews(t.Context(), testOptions.MaxRoomBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		o := roomIn(t, got, w.ID)
		if o.Volume != w.Volume || o.Participants != w.Participants || o.Personas != w.Personas || !o.LastActivity.Equal(w.LastActivity) {
			t.Fatalf("room %s: %+v, store says %+v", w.ID, o, w)
		}
	}
	r := roomIn(t, got, room.ID)
	if r.Participants != 1 || r.Personas != 1 || r.Volume.Uploads != int64(len("# Plan ✓")) || r.Used != r.Volume.Uploads ||
		r.Quota != testOptions.MaxRoomBytes || r.QuotaBytes != 0 || r.HostOnline {
		t.Fatalf("room: %+v", r)
	}
	if q := roomIn(t, got, quiet.ID); q.Volume.Total != 0 || q.Participants != 0 || !q.LastActivity.Equal(q.CreatedAt) {
		t.Fatalf("quiet room: %+v", q)
	}

	// The host's daemon connects: the room shows its host online, then offline once it drops.
	d := e.daemonFor(t, room.ID, "Host")
	e.connect(t, room.ID, d, true)
	e.waitAgent(t, room.ID, alice.Token, func(a agentState) bool { return a.Online })
	if r := roomIn(t, e.rooms(t), room.ID); !r.HostOnline || r.HostParticipantID != d.p.Participant.ID || r.Participants != 2 {
		t.Fatalf("host connected: %+v", r)
	}
	d.drop()
	e.waitAgent(t, room.ID, alice.Token, func(a agentState) bool { return !a.Online })
	if r := roomIn(t, e.rooms(t), room.ID); r.HostOnline {
		t.Fatalf("host gone: %+v", r)
	}
	if code, _ := e.do(t, "GET", "/api/admin/rooms", alice.Token, nil); code != http.StatusUnauthorized {
		t.Fatalf("a member lists rooms: %d", code)
	}
}

// 004-AC9: only the super-admin sets a room's quota, audited; a member gets a 403. Uploads and
// attachments then honour it, and 0 returns the room to the default.
func TestRoomQuotaIsTheSuperAdmins(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	path := "/api/rooms/" + room.ID + "/quota"
	if code, _ := e.do(t, "PUT", path, alice.Token, map[string]any{"quota_bytes": 1 << 30}); code != http.StatusForbidden {
		t.Fatalf("member sets the quota: %d", code)
	}
	if code, _ := e.do(t, "PUT", path, "", map[string]any{"quota_bytes": 1 << 30}); code != http.StatusUnauthorized {
		t.Fatalf("anonymous sets the quota: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/admin/rooms/"+room.ID+"/quota", alice.Token, map[string]any{"quota_bytes": 1 << 30}); code != http.StatusUnauthorized {
		t.Fatalf("member on the admin route: %d", code)
	}
	for _, bad := range []any{map[string]any{}, map[string]any{"quota_bytes": -1}, map[string]any{"quota_bytes": int64(maxQuota) + 1}, map[string]any{"quota_bytes": "1"}} {
		if code, b := e.do(t, "PUT", path, e.admin, bad); code != http.StatusBadRequest {
			t.Errorf("quota %v: %d %s", bad, code, b)
		}
	}
	if code, _ := e.do(t, "PUT", "/api/admin/rooms/nope/quota", e.admin, map[string]any{"quota_bytes": 1}); code != http.StatusNotFound {
		t.Fatalf("unknown room: %d", code)
	}
	if r := roomIn(t, e.rooms(t), room.ID); r.Quota != testOptions.MaxRoomBytes {
		t.Fatalf("refused changes applied: %+v", r)
	}

	const quota = 3000
	if code, b := e.do(t, "PUT", path, e.admin, map[string]any{"quota_bytes": quota}); code != http.StatusNoContent {
		t.Fatalf("admin sets the quota: %d %s", code, b)
	}
	if r := roomIn(t, e.rooms(t), room.ID); r.Quota != quota || r.QuotaBytes != quota {
		t.Fatalf("quota not shown: %+v", r)
	}
	if l := e.auditLog(t, "?action=room.quota"); len(l.Entries) != 1 || l.Entries[0].TargetID != room.ID || l.Entries[0].TargetName != room.Name ||
		!bytes.Contains(l.Entries[0].Details, []byte(`"quota_bytes":3000`)) {
		t.Fatalf("audit: %+v", l.Entries)
	}
	plan := e.uploadDoc(t, room.ID, alice.Token, "plan.md", md(quota-len(png)))
	if code, b := e.upload(t, room.ID, alice.Token, "more.md", md(len(png)+1), nil); code != http.StatusRequestEntityTooLarge ||
		!bytes.Contains(b, []byte("over its 3000 bytes limit")) {
		t.Fatalf("upload past the room's quota: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", annPath(room.ID, plan.ID), alice.Token, map[string]any{"attachments": images(1, append(bytes.Clone(png), 0))}); code != http.StatusRequestEntityTooLarge ||
		!bytes.Contains(b, []byte("over its 3000 bytes limit")) {
		t.Fatalf("attachment past the room's quota: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", annPath(room.ID, plan.ID), alice.Token, map[string]any{"attachments": images(1, png)}); code != http.StatusCreated {
		t.Fatalf("attachment exactly at the quota: %d %s", code, b)
	}
	// Back to the default: the room has room again.
	if code, _ := e.do(t, "PUT", path, e.admin, map[string]any{"quota_bytes": 0}); code != http.StatusNoContent {
		t.Fatalf("reset: %d", code)
	}
	if r := roomIn(t, e.rooms(t), room.ID); r.Quota != testOptions.MaxRoomBytes || r.QuotaBytes != 0 {
		t.Fatalf("after reset: %+v", r)
	}
	e.uploadDoc(t, room.ID, alice.Token, "more.md", md(len(png)+1))
}

// 004-AC2 to 004-AC5: the /admin page's session cookie, which only reaches /api/admin, opens every
// view and action; each action is audited (004-AC6).
func TestAdminViewsTakeTheSessionCookie(t *testing.T) {
	e := newEnv(t)
	e.clock()
	room, alice := e.member(t)
	page := e.device(t)
	if code, _ := page.login(t, e.admin, "203.0.113.9"); code != http.StatusCreated {
		t.Fatal("login")
	}
	for _, path := range []string{"/api/admin/rooms", "/api/admin/profiles", "/api/admin/templates", "/api/admin/skills", "/api/admin/instance",
		"/api/admin/rooms/" + room.ID + "/invites"} {
		if code, b := page.do(t, "GET", path, nil); code != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, code, b)
		}
	}
	code, b := page.do(t, "GET", "/api/admin/rooms/"+room.ID+"/invites", nil)
	invs := decodeJSON[[]store.Invite](t, b)
	if code != http.StatusOK || len(invs) != 1 {
		t.Fatalf("invites: %d %s", code, b)
	}
	steps := []struct {
		method, path string
		body         any
		want         int
		action       string
	}{
		{"PATCH", "/api/admin/rooms/" + room.ID, map[string]string{"name": "Renamed"}, http.StatusOK, ActionRoomRename},
		{"PUT", "/api/admin/rooms/" + room.ID + "/quota", map[string]any{"quota_bytes": 1 << 20}, http.StatusNoContent, ActionRoomQuota},
		{"POST", "/api/admin/rooms/" + room.ID + "/invites", nil, http.StatusCreated, ActionInviteCreate},
		{"DELETE", "/api/admin/rooms/" + room.ID + "/invites/" + itoa(invs[0].ID), nil, http.StatusOK, ActionInviteRevoke},
		{"DELETE", "/api/admin/rooms/" + room.ID, nil, http.StatusNoContent, ActionRoomDelete},
	}
	for _, s := range steps {
		if code, b := page.do(t, s.method, s.path, s.body); code != s.want {
			t.Fatalf("%s %s: %d %s", s.method, s.path, code, b)
		}
		// The newest entry: the setup's invite was the token's.
		if l := e.auditLog(t, "?action="+s.action); len(l.Entries) == 0 || l.Entries[0].TargetID != room.ID || !bytes.Contains(l.Entries[0].Details, []byte(`"via":"session"`)) {
			t.Fatalf("%s audit: %+v", s.action, l.Entries)
		}
	}
	if code, _ := page.do(t, "DELETE", "/api/admin/rooms/"+room.ID, nil, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site delete: %d", code)
	}
	// The cookie is the /admin page's: a member's credentials open none of it.
	for _, path := range []string{"/api/admin/rooms", "/api/admin/profiles", "/api/admin/instance"} {
		if code, _ := e.do(t, "GET", path, alice.Token, nil); code != http.StatusUnauthorized {
			t.Errorf("member GET %s: %d", path, code)
		}
	}
	if !strings.Contains(strings.Join(AuditActions, ","), ActionRoomQuota) {
		t.Fatal("room.quota missing from the audit filter")
	}
}
