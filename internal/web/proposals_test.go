package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// proposalRoom is a turn room where Ana is a room admin and Bob a plain member, both with a
// profile; Alice hosts and presses.
type proposalRoom struct {
	*turnRoom
	ana, bob       *device
	anaPID, bobPID int64
}

func newProposalRoom(t *testing.T, e *env) *proposalRoom {
	t.Helper()
	r := &proposalRoom{turnRoom: newTurnRoom(t, e), ana: e.device(t), bob: e.device(t)}
	r.anaPID = r.ana.join(t, r.room, "Ana").Participant.ID
	r.bobPID = r.bob.join(t, r.room, "Bob").Participant.ID
	if code, b := e.do(t, "PUT", fmt.Sprintf("/api/admin/rooms/%s/admins/%d", r.room, r.anaPID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("admin: %d %s", code, b)
	}
	return r
}

// propose runs a turn of the room's persona whose result carries roomNew as the daemon posts it,
// and returns the room's lines after the synthesis.
func (r *proposalRoom) propose(t *testing.T, e *env, roomNew string) []store.Message {
	t.Helper()
	tn := e.press(t, r.turnRoom, r.alice, r.alice.p.Token, false)
	res := turn.Result{Synthesis: "See the card.", SessionID: sid1, Session: []byte("x\n"), RoomNew: roomNew}
	if data, err := e.st.AgentSession(t.Context(), r.room, tn.PersonaID, sid1); err == nil {
		res.SessionOffset = int64(len(data))
	}
	if code, b := e.result(t, r.turnRoom, r.alice, tn.ID, res); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	msgs, err := e.st.Messages(t.Context(), r.room, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(msgs, func(m store.Message) bool { return m.TurnID == tn.ID && m.Kind == store.KindAgent })
	if i < 0 {
		t.Fatalf("no synthesis in %+v", msgs)
	}
	return msgs[i+1:]
}

// card proposes name and returns its card.
func (r *proposalRoom) card(t *testing.T, e *env, name string) store.Message {
	t.Helper()
	after := r.propose(t, e, fmt.Sprintf(`{"name": %q, "brief": "# %s\n\nWhy a room <script>x</script>."}`, name, name))
	if len(after) != 1 || after[0].Proposal == nil {
		t.Fatalf("after the synthesis: %+v", after)
	}
	return after[0]
}

func proposalPath(room string, id int64, action string) string {
	return fmt.Sprintf("/api/rooms/%s/proposals/%d/%s", room, id, action)
}

// 023-AC16: a valid room-new.json posts a card under the synthesis, its brief rendered safely; each
// refused one is listed in the turn's system line, with no card.
func TestRoomNewResult(t *testing.T) {
	e := newEnv(t)
	r := newProposalRoom(t, e)
	c := r.card(t, e, "Legal review")
	if c.Kind != store.KindSystem || c.Body != "Grill proposes a room: Legal review" || c.Proposal.Name != "Legal review" ||
		c.Proposal.Status != store.ProposalOpen || c.Proposal.Trigger != r.alice.p.Participant.ID {
		t.Fatalf("card %+v %+v", c, c.Proposal)
	}
	if !strings.Contains(c.HTML, "<h1>Legal review</h1>") || strings.Contains(c.HTML, "<script>") {
		t.Fatalf("brief html %q", c.HTML)
	}
	for name, file := range map[string]string{
		"bad json":       `{"name": "Legal"`,
		"second":         `[{"name": "Legal", "brief": ""}, {"name": "Tax", "brief": ""}]`,
		"empty name":     `{"name": "  ", "brief": "x"}`,
		"multi-line":     `{"name": "Legal\nTax", "brief": "x"}`,
		"81 characters":  `{"name": "` + strings.Repeat("é", turn.MaxRoomNewName+1) + `", "brief": "x"}`,
		"brief too long": `{"name": "Legal", "brief": "` + strings.Repeat("a", turn.MaxRoomNewBrief+1) + `"}`,
	} {
		after := r.propose(t, e, file)
		if len(after) != 1 || after[0].Proposal != nil || !strings.Contains(after[0].Body, "ignored: room-new.json: ") {
			t.Errorf("%s: after the synthesis %+v", name, after)
		}
	}
	var n int
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT count(*) FROM room_proposals`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("proposals stored: %d %v", n, err)
	}
}

// 023-AC17 023-AC18 023-AC19 023-AC20 023-AC21: a room admin creates the room from the card with a
// member and both links: brief.md, seats through her shareable invite, a push, the card updated,
// "proposed in" kept once the first room is gone; the second action answers 409.
func TestCreateFromProposal(t *testing.T) {
	b := newPushBrowser(t)
	e := pushEnv(t, b)
	r := newProposalRoom(t, e)
	c := r.card(t, e, "Legal")
	// Both subscribe; only Bob, seated by Ana, hears of the new room.
	for _, d := range []*device{r.ana, r.bob} {
		if code, body := d.do(t, "PUT", "/api/rooms/"+r.room+"/push", map[string]any{"subscription": b.subscription(fmt.Sprintf("/%p", d))}); code != http.StatusNoContent {
			t.Fatalf("subscribe: %d %s", code, body)
		}
	}
	read := r.ana.newToken(t, "read", 0)
	for _, action := range []string{"create", "dismiss"} {
		if code, body := e.do(t, "POST", proposalPath(r.room, c.Proposal.ID, action), read.Token, map[string]any{"name": "Legal"}); code != http.StatusForbidden || errorOf(t, body) != msgTokenReadOnly {
			t.Fatalf("read-only %s: %d %s", action, code, body)
		}
	}
	if code, _ := r.ana.do(t, "POST", proposalPath(r.room, 999999, "create"), map[string]any{"name": "Legal"}); code != http.StatusNotFound {
		t.Fatalf("unknown proposal: %d", code)
	}
	if code, _ := r.ana.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "create"), map[string]any{"name": " "}); code != http.StatusBadRequest {
		t.Fatalf("empty name: %d", code)
	}
	// L1: a sane cap on the members one creation seats.
	if code, body := r.ana.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "create"),
		map[string]any{"name": "Legal", "members": make([]int64, store.MaxProposalMembers+1)}); code != http.StatusBadRequest {
		t.Fatalf("too many members: %d %s", code, body)
	}
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET max_rooms = 20`); err != nil {
		t.Fatal(err)
	}
	// A proposal is acted on through its own room only; a seat of another room is never seated.
	other := r.ana.createRoom(t, "Other", "Ana")
	carl := e.device(t).join(t, other.ID, "Carl").Participant.ID
	for _, action := range []string{"create", "dismiss"} {
		if code, body := r.ana.do(t, "POST", proposalPath(other.ID, c.Proposal.ID, action), map[string]any{"name": "Legal"}); code != http.StatusNotFound {
			t.Fatalf("%s through another room: %d %s", action, code, body)
		}
	}
	code, body := r.ana.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "create"),
		map[string]any{"name": "Legal team", "members": []int64{r.bobPID, r.anaPID, r.bobPID, carl}, "reads": true, "read_by": true})
	room := decodeJSON[store.Room](t, body)
	if code != http.StatusCreated || room.ProposedIn == nil || room.ProposedIn.Room != r.room || room.ProposedIn.Name != "Grill" {
		t.Fatalf("create: %d %s", code, body)
	}
	ctx := t.Context()
	docs, err := e.st.Documents(ctx, room.ID)
	if err != nil || len(docs) != 1 || docs[0].Name != store.BriefDoc || docs[0].Author != "Ana" {
		t.Fatalf("documents %+v %v", docs, err)
	}
	if rev, err := e.st.Revision(ctx, docs[0].ID, 1); err != nil || !strings.HasPrefix(rev.Body, "# Legal\n") {
		t.Fatalf("brief %+v %v", rev, err)
	}
	// Ana is creator and room admin; Bob sits once, through Ana's shareable invite.
	var creator int64
	var shareable, bobSeats int
	db := e.sqlDB(t)
	if err := db.QueryRowContext(ctx, `SELECT creator_profile_id, (SELECT count(*) FROM participants p JOIN invites i ON i.id = p.invite_id
		JOIN participants c ON c.id = i.created_by WHERE p.room_id = r.id AND p.name = 'Bob' AND i.link IS NOT NULL AND c.name = 'Ana'),
		(SELECT count(*) FROM participants WHERE room_id = r.id AND name IN ('Bob', 'Carl')) FROM rooms r WHERE id = $1`, room.ID).Scan(&creator, &shareable, &bobSeats); err != nil {
		t.Fatal(err)
	}
	ana := r.ana.profile(t).Profile
	if creator != ana.ID || shareable != 1 || bobSeats != 1 {
		t.Fatalf("creator %d (Ana %d), Bob through the invite %d, Bob seats %d", creator, ana.ID, shareable, bobSeats)
	}
	if _, err := e.st.RoomAdmin(ctx, room.ID, ana.ID); err != nil {
		t.Fatalf("Ana is no room admin: %v", err)
	}
	links, err := e.st.RoomLinks(ctx, room.ID, ana.ID)
	if err != nil || len(links) != 2 {
		t.Fatalf("links %+v %v", links, err)
	}
	if got := e.lastLine(t, r.room); got.Body != "Ana linked this room: it now reads Legal team" {
		t.Fatalf("last line in Grill: %+v", got)
	}
	card, err := e.st.Message(ctx, r.room, c.ID)
	if err != nil || card.Body != "Created by Ana: Legal team" || card.Proposal.Status != store.ProposalCreated || card.Proposal.Room != room.ID || card.HTML != "" {
		t.Fatalf("card after %+v %v", card, err)
	}
	es := e.auditLog(t, "?action="+ActionRoomCreate).Entries
	var d map[string]any
	if len(es) == 0 || es[0].TargetID != room.ID || json.Unmarshal(es[0].Details, &d) != nil || d["proposal"] != float64(c.Proposal.ID) || d["proposed_in"] != r.room {
		t.Fatalf("audit %+v", es)
	}
	if ls := e.auditLog(t, "?action="+ActionRoomLinkCreate).Entries; len(ls) != 2 {
		t.Fatalf("link audit %+v", ls)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(b.pushes(), func(p pushPayload) bool { return p.Kind == pushAdded }) {
		if time.Now().After(deadline) {
			t.Fatalf("no push for Bob: %+v", b.pushes())
		}
		time.Sleep(pushTick) // polls the fake push service, bounded by the deadline
	}
	time.Sleep(10 * pushTick) // a second push, Ana's by mistake, would land meanwhile
	var added []pushPayload
	for _, p := range b.pushes() {
		if p.Kind == pushAdded {
			added = append(added, p)
		}
	}
	if len(added) != 1 || added[0].Room != "Legal team" || !strings.HasPrefix(added[0].URL, "/r/"+room.ID+"#m") {
		t.Fatalf("pushes %+v", added)
	}

	// 023-AC19: acted on once.
	for _, c2 := range []struct {
		dev    *device
		action string
	}{{r.ana, "create"}, {r.ana, "dismiss"}, {r.bob, "create"}} {
		if code, body := c2.dev.do(t, "POST", proposalPath(r.room, c.Proposal.ID, c2.action), map[string]any{"name": "Again"}); code != http.StatusConflict || errorOf(t, body) != msgHandled {
			t.Errorf("%s after create: %d %s", c2.action, code, body)
		}
	}

	// 023-AC18: Bob looking at the room sees the card change; no push goes to his focused tab.
	c3 := r.card(t, e, "Ops")
	if code, body := r.bob.do(t, "POST", "/api/rooms/"+r.room+"/presence", map[string]any{"tab": "t1", "focused": true}); code != http.StatusNoContent {
		t.Fatalf("presence: %d %s", code, body)
	}
	if code, body := r.ana.do(t, "POST", proposalPath(r.room, c3.Proposal.ID, "create"), map[string]any{"name": "Ops", "members": []int64{r.bobPID}}); code != http.StatusCreated {
		t.Fatalf("create Ops: %d %s", code, body)
	}
	time.Sleep(10 * pushTick) // a push to the focused tab would land meanwhile
	if n := len(slices.DeleteFunc(b.pushes(), func(p pushPayload) bool { return p.Kind != pushAdded })); n != 1 {
		t.Fatalf("added pushes after a focused member: %d", n)
	}

	// 023-AC20: the new room shows where it was proposed, linked for a member, the name kept once it is gone.
	code, body = r.ana.do(t, "GET", "/api/rooms/"+room.ID+"/overview", nil)
	if o := decodeJSON[store.RoomOverview](t, body); code != http.StatusOK || o.ProposedIn == nil || !o.ProposedIn.Member || o.ProposedIn.Name != "Grill" {
		t.Fatalf("overview: %d %s", code, body)
	}
	if _, err := e.st.DeleteRoom(ctx, r.room); err != nil {
		t.Fatal(err)
	}
	code, body = r.ana.do(t, "GET", "/api/rooms/"+room.ID, nil)
	var v struct {
		Room store.Room `json:"room"`
	}
	if err := json.Unmarshal(body, &v); code != http.StatusOK || err != nil || v.Room.ProposedIn == nil || v.Room.ProposedIn.Room != "" || v.Room.ProposedIn.Name != "Grill" {
		t.Fatalf("room after Grill went: %d %s", code, body)
	}
}

// 023-AC18 023-AC19: a plain member gets no links whatever they send; limits refuse with nothing
// created; dismiss belongs to who started the turn and room admins; two creates at once make one room.
func TestProposalRightsAndLimits(t *testing.T) {
	e := newEnv(t)
	r := newProposalRoom(t, e)
	ctx := t.Context()
	db := e.sqlDB(t)
	rooms := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM rooms`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Limits: Bob may create no room; nothing is created and the card stays open.
	c := r.card(t, e, "Tax")
	bobProfile := r.bob.profile(t).Profile.ID
	r.bob.createRoom(t, "Mine", "Bob")
	if _, err := db.ExecContext(ctx, `UPDATE profiles SET max_rooms = 1 WHERE id = $1`, bobProfile); err != nil {
		t.Fatal(err)
	}
	before := rooms()
	code, body := r.bob.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "create"), map[string]any{"name": "Tax", "members": []int64{r.anaPID}})
	if code != http.StatusForbidden || !strings.Contains(errorOf(t, body), "you have created 1 rooms") || rooms() != before {
		t.Fatalf("over the limit: %d %s, rooms %d then %d", code, body, before, rooms())
	}
	if m, _ := e.st.Message(ctx, r.room, c.ID); m.Body != c.Body || m.Proposal.Status != store.ProposalOpen {
		t.Fatalf("card after a refusal %+v", m)
	}
	if _, err := db.ExecContext(ctx, `UPDATE profiles SET max_rooms = NULL WHERE id = $1`, bobProfile); err != nil {
		t.Fatal(err)
	}
	// A plain member's link flags are dropped.
	code, body = r.bob.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "create"), map[string]any{"name": "Tax", "reads": true, "read_by": true})
	room := decodeJSON[store.Room](t, body)
	if code != http.StatusCreated {
		t.Fatalf("member creates: %d %s", code, body)
	}
	if links, err := e.st.RoomLinks(ctx, room.ID, bobProfile); err != nil || len(links) != 0 {
		t.Fatalf("a member linked: %+v %v", links, err)
	}

	// Dismiss: another member is refused; who pressed and a room admin may.
	c = r.card(t, e, "Ops")
	if code, body := r.bob.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "dismiss"), nil); code != http.StatusForbidden || errorOf(t, body) != msgProposalDismiss {
		t.Fatalf("member dismisses: %d %s", code, body)
	}
	if code, body := e.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "dismiss"), r.alice.p.Token, nil); code != http.StatusNoContent {
		t.Fatalf("who pressed dismisses: %d %s", code, body)
	}
	if m, _ := e.st.Message(ctx, r.room, c.ID); m.Body != "Dismissed by Alice: Ops" || m.Proposal.Status != store.ProposalDismissed {
		t.Fatalf("dismissed card %+v", m)
	}
	if code, body := r.ana.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "create"), map[string]any{"name": "Ops"}); code != http.StatusConflict || errorOf(t, body) != msgHandled {
		t.Fatalf("create after dismiss: %d %s", code, body)
	}
	c = r.card(t, e, "Docs")
	if code, body := r.ana.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "dismiss"), nil); code != http.StatusNoContent {
		t.Fatalf("room admin dismisses: %d %s", code, body)
	}
	if es := e.auditLog(t, "?action="+ActionProposalDismiss).Entries; len(es) != 2 {
		t.Fatalf("dismiss audit %+v", es)
	}

	// A member without a profile creates nothing.
	c = r.card(t, e, "Race")
	bare := e.join(t, e.createInvite(t, r.room).Token, "Nora")
	if _, err := db.ExecContext(ctx, `UPDATE participants SET profile_id = NULL WHERE id = $1`, bare.Participant.ID); err != nil {
		t.Fatal(err)
	}
	if code, b := e.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "create"), bare.Token, map[string]any{"name": "Race"}); code != http.StatusForbidden || errorOf(t, b) != msgProposalProfile {
		t.Fatalf("no profile: %d", code)
	}
	// Two creates at once: one room, one 409.
	before = rooms()
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, d := range []*device{r.ana, r.bob} {
		wg.Go(func() {
			codes[i], _ = d.do(t, "POST", proposalPath(r.room, c.Proposal.ID, "create"), map[string]any{"name": "Race"})
		})
	}
	wg.Wait()
	slices.Sort(codes)
	if codes[0] != http.StatusCreated || codes[1] != http.StatusConflict || rooms() != before+1 {
		t.Fatalf("racing creates: %v, rooms %d then %d", codes, before, rooms())
	}
}

// 021-AC6 023-AC18: on a restricted instance only an internal creates the proposed room.
func TestProposalRestricted(t *testing.T) {
	e, o, room, actors := restrictedRoom(t)
	e.srv.opts.OIDC = o
	var id int64
	if err := e.sqlDB(t).QueryRowContext(context.Background(), `INSERT INTO room_proposals(room_id, name, brief, created_at) VALUES($1, 'Legal', '', '') RETURNING id`, room).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, a := range actors {
		want := map[string]string{"external member": cellCreate, "external room admin": cellCreate, "internal": cellOK}[a.name]
		if want == "" {
			continue
		}
		code, b := a.do(t, e, "POST", proposalPath(room, id, "create"), map[string]any{"name": "Legal"})
		if got := outcome(t, code, b); got != want {
			t.Errorf("%s: %s, want %s", a.name, got, want)
		}
	}
}

// 023-AC4 023-AC18: the proposal routes answer 400 on a malformed body, 409 when a link would pass
// the read cap (nothing created), and 500 when the store fails.
func TestProposalRoutesFail(t *testing.T) {
	e := newEnv(t)
	r := newProposalRoom(t, e)
	c := r.card(t, e, "Legal")
	path := proposalPath(r.room, c.Proposal.ID, "create")
	if code, _ := r.ana.raw(t, "POST", path, "application/json", []byte(`{"name":`)); code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", code)
	}
	if code, _ := r.ana.do(t, "POST", proposalPath(r.room, 999999, "dismiss"), nil); code != http.StatusNotFound {
		t.Fatalf("unknown dismiss: %d", code)
	}
	db := e.sqlDB(t)
	// The room already reads 10 others: "this room reads the new one" passes the cap.
	for i := range store.MaxRoomReads {
		id := fmt.Sprintf("other%d", i)
		if _, err := e.st.CreateRoom(t.Context(), id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO room_links(reader_room_id, read_room_id, created_at) VALUES($1, $2, '')`, r.room, id); err != nil {
			t.Fatal(err)
		}
	}
	var rooms int
	_ = db.QueryRowContext(t.Context(), `SELECT count(*) FROM rooms`).Scan(&rooms)
	code, body := r.ana.do(t, "POST", path, map[string]any{"name": "Legal", "read_by": true})
	var after int
	_ = db.QueryRowContext(t.Context(), `SELECT count(*) FROM rooms`).Scan(&after)
	if code != http.StatusConflict || !strings.Contains(errorOf(t, body), "already reads 10 rooms") || after != rooms {
		t.Fatalf("past the cap: %d %s, rooms %d then %d", code, body, rooms, after)
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE room_proposals RENAME TO gone`); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"create", "dismiss"} {
		if code, body := r.ana.do(t, "POST", proposalPath(r.room, c.Proposal.ID, action), map[string]any{"name": "Legal"}); code != http.StatusInternalServerError {
			t.Errorf("%s with the store failing: %d %s", action, code, body)
		}
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE gone RENAME TO room_proposals`); err != nil {
		t.Fatal(err)
	}
}

// 023-AC18: the "added" pushes run on the pod's context: a stopped pod sends none.
func TestPushAddedEndsWithThePod(t *testing.T) {
	b := newPushBrowser(t)
	e := pushEnv(t, b)
	room := e.createRoom(t, "Pricing")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	if code, body := e.do(t, "PUT", "/api/rooms/"+room.ID+"/push", alice.Token, map[string]any{"subscription": b.subscription("/a")}); code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	seats := []store.Seated{{From: alice.Participant.ID, Line: 1}}
	stopped, stop := context.WithCancel(t.Context())
	stop()
	e.srv.pushAdded(stopped, room.ID, room.ID, seats)
	time.Sleep(10 * pushTick) // a push despite the stop would land meanwhile
	if got := b.pushes(); len(got) != 0 {
		t.Fatalf("pushes after the pod stopped: %+v", got)
	}
	e.srv.pushAdded(t.Context(), room.ID, room.ID, seats)
	deadline := time.Now().Add(5 * time.Second)
	for len(b.pushes()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no push while the pod runs")
		}
		time.Sleep(pushTick) // polls the fake push service, bounded by the deadline
	}
}
