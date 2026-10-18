package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

func (e *env) setSettings(t *testing.T, v store.InstanceSettings) {
	t.Helper()
	if code, b := e.do(t, "PUT", "/api/admin/settings", e.admin, v); code != http.StatusOK {
		t.Fatalf("settings: %d %s", code, b)
	}
}

// 004-AC7: a profile creates rooms from its profile page, as their room admin, within the
// guardrails the super-admin sets in /admin; each refusal says why.
func TestProfileCreatesRooms(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	if code, _ := alice.do(t, "POST", "/api/profile/rooms", map[string]string{"name": "R", "display_name": "Alice"}); code != http.StatusUnauthorized {
		t.Fatalf("without a profile: %d", code)
	}
	room := alice.createRoom(t, "Mine", "Ali")
	code, b := alice.do(t, "GET", "/api/rooms/"+room.ID, nil)
	if got := decodeJSON[roomResp](t, b); code != http.StatusOK || got.Me.Name != "Ali" || !got.Me.Admin || len(got.Participants) != 1 || !got.Participants[0].Admin {
		t.Fatalf("the creator in the room: %d %s", code, b)
	}
	if prof := alice.profile(t).Profile; prof.Rooms[0].RoomID != room.ID {
		t.Fatalf("profile rooms: %+v", prof.Rooms)
	}
	for _, c := range []struct {
		body map[string]string
		hdr  []string
		want int
	}{
		{map[string]string{"name": "", "display_name": "A"}, nil, http.StatusBadRequest},
		{map[string]string{"name": "R", "display_name": strings.Repeat("x", maxNameLen+1)}, nil, http.StatusBadRequest},
		{map[string]string{"name": "R", "display_name": "A"}, []string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
	} {
		if code, b := alice.do(t, "POST", "/api/profile/rooms", c.body, c.hdr...); code != c.want {
			t.Errorf("%v %v: %d %s, want %d", c.body, c.hdr, code, b, c.want)
		}
	}

	if code, b := e.do(t, "GET", "/api/admin/settings", e.admin, nil); code != http.StatusOK ||
		decodeJSON[store.InstanceSettings](t, b) != (store.InstanceSettings{RoomsPerProfile: 10, RoomCreation: store.CreationAnyone, PluginMaxMB: store.DefaultPluginMaxMB, PurgeEmptyProfiles: true}) {
		t.Fatalf("default settings: %d %s", code, b)
	}
	for _, bad := range []store.InstanceSettings{
		{RoomsPerProfile: 0, RoomCreation: store.CreationAnyone},
		{RoomsPerProfile: 1001, RoomCreation: store.CreationAnyone},
		{RoomsPerProfile: 1, StorageQuota: -1, RoomCreation: store.CreationAnyone},
		{RoomsPerProfile: 1, RoomCreation: "everyone"},
	} {
		if code, _ := e.do(t, "PUT", "/api/admin/settings", e.admin, bad); code != http.StatusBadRequest {
			t.Errorf("%+v: %d", bad, code)
		}
	}
	if code, _ := alice.do(t, "PUT", "/api/admin/settings", store.InstanceSettings{RoomsPerProfile: 99, RoomCreation: store.CreationAnyone}); code != http.StatusUnauthorized {
		t.Fatalf("a profile sets the guardrails: %d", code)
	}
	refused := func(want string) {
		t.Helper()
		code, b := alice.do(t, "POST", "/api/profile/rooms", map[string]string{"name": "One more", "display_name": "Ali"})
		if code != http.StatusForbidden || !strings.Contains(string(b), want) {
			t.Fatalf("refusal: %d %s, want %q", code, b, want)
		}
	}
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 1, RoomCreation: store.CreationAnyone})
	refused("room creation refused: you have created 1 rooms, the most this instance allows per profile")
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 5, RoomCreation: store.CreationSuperAdmin})
	refused("room creation refused: only the super-admin creates rooms on this instance")
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 5, RoomCreation: store.CreationAnyone, StorageQuota: 1})
	refused("storage quota")
	// The super-admin is not bound by them.
	e.createRoom(t, "Admin's")

	log := e.auditLog(t, "?action="+ActionSettings).Entries
	if len(log) != 3 || log[0].TargetKind != store.TargetInstance || !strings.Contains(string(log[0].Details), `"storage_quota_bytes":1`) {
		t.Fatalf("settings audit: %+v", log)
	}
	created := e.auditLog(t, "?action="+ActionRoomCreate+"&target="+room.ID).Entries
	if len(created) != 1 || !strings.HasPrefix(created[0].Actor, ActorProfile) || !strings.Contains(string(created[0].Details), `"as":"Ali"`) {
		t.Fatalf("creation audit: %+v", created)
	}
}

// 004-AC8: a room admin renames, sees and revokes every invite, appoints and removes room admins
// but cannot leave the room without one, sees the volume and deletes the room. A plain member keeps
// only their own invites. Every room admin action is audited under the acting profile.
func TestRoomAdmins(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Pricing", "Alice")
	path := "/api/rooms/" + room.ID
	bob := e.device(t)
	bobP := bob.join(t, room.ID, "Bob").Participant
	carol := e.join(t, e.createInvite(t, room.ID).Token, "Carol")
	code, b := bob.do(t, "POST", path+"/invites", nil)
	if code != http.StatusCreated {
		t.Fatalf("bob invites: %d %s", code, b)
	}
	bobInvite := decodeJSON[inviteResp](t, b)
	_, b = e.do(t, "GET", path+"/invites", e.admin, nil)
	aliceSeat := decodeJSON[[]store.Invite](t, b)[0].ID

	// Bob, a plain member: refused everything but his own invites.
	for _, c := range []struct{ method, path string }{
		{"PATCH", path}, {"DELETE", path}, {"GET", path + "/overview"},
		{"PUT", fmt.Sprintf("%s/admins/%d", path, bobP.ID)}, {"DELETE", fmt.Sprintf("%s/admins/%d", path, carol.Participant.ID)},
		{"PUT", path + "/quota"}, {"DELETE", fmt.Sprintf("%s/invites/%d", path, aliceSeat)},
	} {
		if code, b := bob.do(t, c.method, c.path, map[string]any{"name": "x", "quota_bytes": 1}); code != http.StatusForbidden {
			t.Errorf("bob %s %s: %d %s", c.method, c.path, code, b)
		}
	}
	if _, b := bob.do(t, "GET", path+"/invites", nil); len(decodeJSON[[]store.Invite](t, b)) != 1 {
		t.Fatalf("bob's invites: %s", b)
	}

	// Alice, the room admin.
	if code, b := alice.do(t, "PUT", path+"/quota", map[string]any{"quota_bytes": 1}); code != http.StatusForbidden {
		t.Fatalf("room admin sets the quota: %d %s", code, b)
	}
	code, b = alice.do(t, "GET", path+"/overview", nil)
	if ov := decodeJSON[roomOverview](t, b); code != http.StatusOK || ov.ID != room.ID || ov.Participants != 3 || ov.Quota != testOptions.MaxRoomBytes || ov.Volume.Transcripts == 0 {
		t.Fatalf("overview: %d %s", code, b)
	}
	// Four, plus alice's shareable link made with the room (011-AC26).
	if _, b := alice.do(t, "GET", path+"/invites", nil); len(decodeJSON[[]store.Invite](t, b)) != 5 {
		t.Fatalf("every invite: %s", b)
	}
	if code, b := alice.do(t, "DELETE", fmt.Sprintf("%s/invites/%d", path, bobInvite.ID), nil); code != http.StatusOK {
		t.Fatalf("revoke bob's invite: %d %s", code, b)
	}
	if code, b := alice.do(t, "PATCH", path, map[string]string{"name": "Pricing v2"}); code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, b)
	}
	admins := func(want ...string) {
		t.Helper()
		_, b := alice.do(t, "GET", path, nil)
		var got []string
		for _, p := range decodeJSON[roomResp](t, b).Participants {
			if p.Admin {
				got = append(got, p.Name)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("admins %v, want %v", got, want)
		}
	}
	me := decodeJSON[roomResp](t, func() []byte { _, b := alice.do(t, "GET", path, nil); return b }()).Me
	if code, b := alice.do(t, "DELETE", fmt.Sprintf("%s/admins/%d", path, me.ID), nil); code != http.StatusConflict || !strings.Contains(string(b), "last admin") {
		t.Fatalf("last admin steps down: %d %s", code, b)
	}
	if code, b := alice.do(t, "PUT", fmt.Sprintf("%s/admins/%d", path, bobP.ID), nil); code != http.StatusNoContent {
		t.Fatalf("appoint bob: %d %s", code, b)
	}
	admins("Alice", "Bob")
	// Bob, now a room admin, lists every invite and may remove Alice.
	if _, b := bob.do(t, "GET", path+"/invites", nil); len(decodeJSON[[]store.Invite](t, b)) != 5 { // alice's shareable link included (011-AC26)
		t.Fatalf("bob's view as an admin: %s", b)
	}
	if code, _ := bob.do(t, "DELETE", fmt.Sprintf("%s/admins/%d", path, me.ID), nil); code != http.StatusNoContent {
		t.Fatalf("bob removes alice: %d", code)
	}
	admins("Bob")
	if code, _ := alice.do(t, "PATCH", path, map[string]string{"name": "x"}); code != http.StatusForbidden {
		t.Fatalf("former admin renames: %d", code)
	}
	if code, _ := bob.do(t, "PUT", fmt.Sprintf("%s/admins/%d", path, me.ID), nil); code != http.StatusNoContent {
		t.Fatalf("bob appoints alice back: %d", code)
	}
	if code, _ := alice.do(t, "PUT", path+"/admins/999", nil); code != http.StatusNotFound {
		t.Fatalf("unknown participant: %d", code)
	}
	if code, _ := alice.do(t, "PUT", path+"/admins/x", nil); code != http.StatusNotFound {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := alice.do(t, "DELETE", fmt.Sprintf("%s/admins/%d", path, bobP.ID), nil); code != http.StatusNoContent {
		t.Fatalf("alice removes bob: %d", code)
	}
	// A participant whose profile is gone cannot be appointed.
	if code, _ := e.do(t, "DELETE", "/api/admin/profiles/"+strconv.FormatInt(e.participantProfile(t, carol.Participant.ID), 10), e.admin, nil); code != http.StatusNoContent {
		t.Fatal("delete carol's profile")
	}
	if code, b := alice.do(t, "PUT", fmt.Sprintf("%s/admins/%d", path, carol.Participant.ID), nil); code != http.StatusConflict {
		t.Fatalf("appoint a participant without a profile: %d %s", code, b)
	}
	if code, b := alice.do(t, "DELETE", path, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}

	// Each action above, under the profile that did it, newest first.
	var got []string
	for _, en := range e.auditLog(t, "?target="+room.ID).Entries {
		if en.Actor != store.ActorSuperAdmin {
			who := map[bool]string{true: "alice", false: "bob"}[strings.Contains(string(en.Details), `"as":"Alice"`)]
			got = append(got, who+" "+en.Action)
		}
	}
	want := []string{"alice room.delete", "alice room_admin.remove", "bob room_admin.add", "bob room_admin.remove", "alice room_admin.add",
		"alice room.rename", "alice invite.revoke", "alice room.create"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit:\n got %v\nwant %v", got, want)
	}
}

func (e *env) participantProfile(t *testing.T, id int64) int64 {
	t.Helper()
	ps, err := e.st.Profiles(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		for _, r := range p.Rooms {
			if r.ParticipantID == id {
				return p.ID
			}
		}
	}
	t.Fatalf("no profile for participant %d", id)
	return 0
}

// 004-AC10: a member copies a personal skill into the room; under "approve everything" it waits
// for the host, who decides; a room admin removes it; the agent state lists the room's skills.
func TestRoomSkillsAPI(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	bob := e.device(t)
	bob.join(t, r.room, "Bob")
	sk := bob.uploadSkill(t, "personal", skillZip(t, "stamp", "x"), http.StatusCreated)
	path := "/api/rooms/" + r.room + "/skills"
	if code, _ := bob.do(t, "POST", path, map[string]int64{"skill_id": 999}); code != http.StatusNotFound {
		t.Fatalf("unknown skill: %d", code)
	}
	if code, _ := e.do(t, "POST", path, r.alice.p.Token, map[string]int64{"skill_id": sk.ID}); code != http.StatusNotFound {
		t.Fatalf("someone else's skill: %d", code)
	}
	e.settings(t, r.room, r.alice, store.DaemonSettings{ApproveAll: true})
	code, b := bob.do(t, "POST", path, map[string]int64{"skill_id": sk.ID})
	k := decodeJSON[store.Skill](t, b)
	if code != http.StatusAccepted || !k.Pending || k.Scope != store.ScopeRoom {
		t.Fatalf("held copy: %d %s", code, b)
	}
	if code, _ := bob.do(t, "POST", path, map[string]int64{"skill_id": sk.ID}); code != http.StatusConflict {
		t.Fatalf("copied twice: %d", code)
	}
	st := e.agent(t, r.room, r.alice.p.Token)
	if len(st.RoomSkills) != 1 || !st.RoomSkills[0].Pending {
		t.Fatalf("agent state: %+v", st.RoomSkills)
	}
	decision := fmt.Sprintf("%s/%d/decision", path, k.ID)
	if code, _ := bob.do(t, "POST", decision, map[string]bool{"approve": true}); code != http.StatusForbidden {
		t.Fatalf("bob decides: %d", code)
	}
	if code, b := e.do(t, "POST", decision, r.alice.p.Token, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("host approves: %d %s", code, b)
	}
	_, b = bob.do(t, "GET", path, nil)
	if lib := decodeJSON[struct{ Skills []store.Skill }](t, b).Skills; !strings.Contains(fmt.Sprint(lib), "room") {
		t.Fatalf("library: %s", b)
	}
	// Removing is a room admin's: none in a super-admin's room, so only the super-admin's route.
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("%s/%d", path, k.ID), r.alice.p.Token, nil); code != http.StatusForbidden {
		t.Fatalf("plain member removes: %d", code)
	}
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("/api/admin/skills/%d", k.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("super-admin removes: %d", code)
	}

	// In a room of her own, Alice's removal is audited as a room admin action.
	alice := e.device(t)
	own := alice.createRoom(t, "Own", "Alice")
	mine := alice.uploadSkill(t, "personal", skillZip(t, "mine", "y"), http.StatusCreated)
	code, b = alice.do(t, "POST", "/api/rooms/"+own.ID+"/skills", map[string]int64{"skill_id": mine.ID})
	if code != http.StatusCreated {
		t.Fatalf("copy: %d %s", code, b)
	}
	rk := decodeJSON[store.Skill](t, b)
	if code, _ := alice.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/skills/%d", own.ID, rk.ID), nil); code != http.StatusNoContent {
		t.Fatalf("room admin removes: %d", code)
	}
	if code, _ := alice.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/skills/%d", own.ID, rk.ID), nil); code != http.StatusNotFound {
		t.Fatalf("removed twice: %d", code)
	}
	if log := e.auditLog(t, "?action="+ActionRoomSkillRemove).Entries; len(log) != 1 || !strings.Contains(string(log[0].Details), `"name":"mine"`) {
		t.Fatalf("audit: %+v", log)
	}
}

// 004-AC6: with the audit log refusing writes, every audited action answers 500 and changes
// nothing: the entry and the change share one transaction.
func TestAuditedActionsFailWithTheAuditLog(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Pricing", "Alice")
	path := "/api/rooms/" + room.ID
	bob := e.device(t)
	bobP := bob.join(t, room.ID, "Bob").Participant
	sk := alice.uploadSkill(t, "personal", skillZip(t, "stamp", "x"), http.StatusCreated)
	code, b := alice.do(t, "POST", path+"/skills", map[string]int64{"skill_id": sk.ID})
	if code != http.StatusCreated {
		t.Fatalf("copy: %d %s", code, b)
	}
	rk := decodeJSON[store.Skill](t, b)
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `CREATE FUNCTION audit_down() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit down'; END $$;
		CREATE TRIGGER audit_down BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_down()`); err != nil {
		t.Fatal(err)
	}
	before, err := e.st.RoomOverview(t.Context(), room.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", path, map[string]string{"name": "Renamed"}},
		{"PUT", fmt.Sprintf("%s/admins/%d", path, bobP.ID), nil},
		{"POST", path + "/invites", nil},
		{"DELETE", fmt.Sprintf("%s/skills/%d", path, rk.ID), nil},
		{"DELETE", path, nil},
		{"POST", "/api/profile/rooms", map[string]string{"name": "Another", "display_name": "Alice"}},
	} {
		if code, b := alice.do(t, c.method, c.path, c.body); code != http.StatusInternalServerError {
			t.Errorf("alice %s %s: %d %s", c.method, c.path, code, b)
		}
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/admin/settings", store.InstanceSettings{RoomsPerProfile: 3, RoomCreation: store.CreationAnyone}},
		{"PUT", path + "/quota", map[string]int64{"quota_bytes": 1 << 20}},
		{"POST", "/api/rooms", map[string]string{"name": "Admin's"}},
	} {
		if code, b := e.do(t, c.method, c.path, e.admin, c.body); code != http.StatusInternalServerError {
			t.Errorf("admin %s %s: %d %s", c.method, c.path, code, b)
		}
	}
	after, err := e.st.RoomOverview(t.Context(), room.ID, 1)
	if err != nil || after.Name != before.Name || after.QuotaBytes != 0 || after.Volume.RoomSkills != before.Volume.RoomSkills {
		t.Fatalf("the room changed: %+v %v", after, err)
	}
	if _, b := alice.do(t, "GET", path, nil); decodeJSON[roomResp](t, b).Participants[1].Admin {
		t.Fatal("bob became an admin")
	}
	if v, _ := e.st.InstanceSettings(t.Context()); v.RoomsPerProfile != store.DefaultRoomsPerProfile {
		t.Fatalf("settings changed: %+v", v)
	}
	if rooms, _ := e.st.Rooms(t.Context()); len(rooms) != 2 {
		t.Fatalf("rooms: %+v", rooms)
	}
	if code, _ := e.do(t, "GET", "/api/admin/settings", e.admin, nil); code != http.StatusOK {
		t.Fatal("settings unreadable")
	}
	// A store gone entirely: the reads fail too.
	_ = e.st.Close()
	for _, p := range []string{"/api/admin/settings"} {
		if code, _ := e.do(t, "GET", p, e.admin, nil); code != http.StatusInternalServerError {
			t.Errorf("%s with no store: %d", p, code)
		}
	}
}

// 004-AC7: the super-admin sets one profile's rooms limit, unlimited, or back to the instance's;
// each change is audited, and the profiles view shows it.
func TestProfileMaxRoomsAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	alice.createRoom(t, "One", "Alice")
	id := strconv.FormatInt(alice.profile(t).Profile.ID, 10)
	path := "/api/admin/profiles/" + id + "/max_rooms"
	for _, bad := range []string{`{}`, `{"max_rooms": 0}`, `{"max_rooms": -2}`, `{"max_rooms": 1001}`, `{"max_rooms": "unlimited"}`, `{"max_rooms": 1.5}`} {
		var body map[string]any
		_ = json.Unmarshal([]byte(bad), &body)
		if code, _ := e.do(t, "PUT", path, e.admin, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _ := alice.do(t, "PUT", path, map[string]any{"max_rooms": 50}); code != http.StatusUnauthorized {
		t.Fatalf("a profile sets its own limit: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/admin/profiles/999/max_rooms", e.admin, map[string]any{"max_rooms": 2}); code != http.StatusNotFound {
		t.Fatalf("unknown profile: %d", code)
	}
	limit := func() *int {
		t.Helper()
		_, b := e.do(t, "GET", "/api/admin/profiles", e.admin, nil)
		for _, p := range decodeJSON[[]store.AdminProfile](t, b) {
			if strconv.FormatInt(p.ID, 10) == id {
				if p.RoomsCreated != 1 {
					t.Fatalf("rooms created: %d", p.RoomsCreated)
				}
				return p.MaxRooms
			}
		}
		t.Fatal("profile not listed")
		return nil
	}
	for _, c := range []struct {
		body any
		want string
	}{{12, "12"}, {-1, "-1"}, {nil, "<nil>"}} {
		if code, b := e.do(t, "PUT", "/api/profiles/"+id+"/max_rooms", e.admin, map[string]any{"max_rooms": c.body}); code != http.StatusNoContent {
			t.Fatalf("%v: %d %s", c.body, code, b)
		}
		got := "<nil>"
		if n := limit(); n != nil {
			got = strconv.Itoa(*n)
		}
		if got != c.want {
			t.Fatalf("after %v: %s", c.body, got)
		}
	}
	log := e.auditLog(t, "?action="+ActionProfileMaxRooms).Entries
	if len(log) != 3 || log[0].TargetID != id || log[0].TargetName != "Alice" || !strings.Contains(string(log[2].Details), `"max_rooms":12`) ||
		!strings.Contains(string(log[0].Details), `"max_rooms":null`) {
		t.Fatalf("audit: %+v", log)
	}
}

// 004-AC8 004-AC6: a room the super-admin created has no room admin; the super-admin lists its
// participants and appoints, then removes, one with a profile from /admin, each audited.
func TestSuperAdminAppointsRoomAdmins(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "Phone test")
	path := "/api/rooms/" + room.ID
	alice := e.device(t)
	ap := alice.join(t, room.ID, "Alice").Participant
	if code, _ := alice.do(t, "PATCH", path, map[string]string{"name": "x"}); code != http.StatusForbidden {
		t.Fatalf("alice renames before being appointed: %d", code)
	}
	adminPath := "/api/admin/rooms/" + room.ID
	if code, _ := alice.do(t, "GET", adminPath+"/participants", nil); code != http.StatusUnauthorized {
		t.Fatalf("a member lists the participants: %d", code)
	}
	code, b := e.do(t, "GET", adminPath+"/participants", e.admin, nil)
	if ps := decodeJSON[[]adminParticipant](t, b); code != http.StatusOK || len(ps) != 1 || ps[0].Client == nil ||
		ps[0].ID != ap.ID || ps[0].Name != "Alice" || !ps[0].HasProfile || ps[0].Admin {
		t.Fatalf("participants: %d %s", code, b)
	}
	if code, b := e.do(t, "PUT", fmt.Sprintf("%s/admins/%d", adminPath, ap.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("appoint alice: %d %s", code, b)
	}
	_, b = e.do(t, "GET", adminPath+"/participants", e.admin, nil)
	if ps := decodeJSON[[]adminParticipant](t, b); !ps[0].Admin {
		t.Fatalf("alice not listed as admin: %s", b)
	}
	if code, b := alice.do(t, "PATCH", path, map[string]string{"name": "Renamed by Alice"}); code != http.StatusOK {
		t.Fatalf("alice renames as a room admin: %d %s", code, b)
	}
	// The bearer token reaches the same action on the room's own path.
	if code, b := e.do(t, "DELETE", fmt.Sprintf("%s/admins/%d", path, ap.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("remove alice: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/api/admin/rooms/nope/participants", e.admin, nil); code != http.StatusNotFound {
		t.Fatalf("unknown room: %d", code)
	}
	if code, _ := e.do(t, "PUT", adminPath+"/admins/999", e.admin, nil); code != http.StatusNotFound {
		t.Fatalf("unknown participant: %d", code)
	}
	var got []string
	for _, en := range e.auditLog(t, "?target="+room.ID).Entries {
		if en.Actor == store.ActorSuperAdmin && strings.HasPrefix(en.Action, "room_admin.") {
			got = append(got, en.Action)
		}
	}
	if want := []string{ActionRoomAdminRemove, ActionRoomAdminAdd}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit %v, want %v", got, want)
	}
}
