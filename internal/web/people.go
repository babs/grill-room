package web

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/babs/grill-room/internal/store"
	"go.uber.org/zap"
)

const (
	// maxTabs bounds the tabs one participant holds open; past it a new tab is refused.
	maxTabs = 20
	// peopleForget is how long a gone participant's last-seen time stays in the presence table; the
	// list falls back on the stored one after.
	peopleForget = time.Hour
)

// presenceGrace is how long a participant stays here after their last sign of life: a reload, a
// background tab or a phone's suspended stream does not flap them (005-AC6). A var for tests.
var presenceGrace = 20 * time.Second

// deviceLapse is how long a tab's device transcription flag holds after its last device beat: the
// worker beats every 1 s, so a stalled or closed pipeline hands the voice back to the server within
// it (010-AC13).
const deviceLapse = 3 * time.Second

var tabID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// sharedTimeout bounds one read or write of the shared state for callers with no context of their own.
const sharedTimeout = 5 * time.Second

// tab is one open room page of a participant, as its row in the presence table: its last sign of
// life and the audio state it reported.
type tab struct {
	beat                    time.Time
	audio, mic, muted, deaf bool
	// focused is the page in the foreground with the focus: no push for this room then (005-AC24).
	focused bool
}

// person is a participant's presence in their room: their live tabs, their last sign of life, since
// when they are here (009-AC36), and whether a tab transcribes on its device (010-AC13).
type person struct {
	tabs        []tab
	last, since time.Time
	device      bool
}

func (t *tab) alive(now time.Time) bool { return now.Sub(t.beat) <= presenceGrace }

func (p *person) alive(now time.Time) bool {
	return now.Sub(p.last) <= presenceGrace || slices.ContainsFunc(p.tabs, func(t tab) bool { return t.alive(now) })
}

// msTime is a stored time in ms, the zero time for 0.
func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// peopleOf groups presence rows by room then participant, as they stand at now.
func peopleOf(rows []store.PresenceRow, now time.Time) map[string]map[int64]*person {
	out := map[string]map[int64]*person{}
	nowMS := now.UnixMilli()
	for _, r := range rows {
		if r.ExpiresMS <= nowMS {
			continue
		}
		room := out[r.Room]
		if room == nil {
			room = map[int64]*person{}
			out[r.Room] = room
		}
		p := room[r.ParticipantID]
		if p == nil {
			p = &person{}
			room[r.ParticipantID] = p
		}
		if r.Tab == "" {
			p.last, p.since = msTime(r.BeatMS), msTime(r.SinceMS)
			continue
		}
		p.device = p.device || nowMS-r.DeviceMS < deviceLapse.Milliseconds()
		p.tabs = append(p.tabs, tab{beat: msTime(r.BeatMS), audio: r.Audio, mic: r.Mic, muted: r.Muted, deaf: r.Deaf, focused: r.Focused})
	}
	return out
}

// people is the presence of every participant, in the presence table every pod shares (005-AC6,
// 017-AC14). A pod's streams read a room's rows through rooms, once per pollEvery.
type people struct {
	st    *store.Store
	log   *zap.Logger
	rooms podCache[[]store.PresenceRow]
	// hands is the hands of a room, read as its rows are (031-AC2).
	hands podCache[[]store.Hand]
}

func newPeople(st *store.Store, log *zap.Logger) *people { return &people{st: st, log: log} }

// warn logs a presence write whose caller goes on without it.
func (pp *people) warn(room string, err error) {
	if err != nil {
		pp.log.Warn("presence_write_failed", zap.String("room", room), zap.Error(err))
	}
}

func (pp *people) times(now time.Time) store.PresenceTimes {
	return store.PresenceTimes{Now: now.UnixMilli(), Grace: presenceGrace.Milliseconds(), Forget: peopleForget.Milliseconds(), Lapse: deviceLapse.Milliseconds()}
}

// write runs a presence write with its own deadline, then drops the pod's copies holding room.
func (pp *people) write(ctx context.Context, room string, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	err := fn(ctx)
	// A presence write may drop a lapsed hand (031-AC6): its copy goes with the rows'.
	pp.forget(room)
	return err
}

// rows returns the presence rows of room, of every room for "", from the pod's copy, read at most
// once per pollEvery; a failed read keeps the last copy.
// ponytail: a room's rows are read whole once per pollEvery per pod; NOTIFY the changed rows instead
// if rooms grow to hundreds of people.
func (pp *people) rows(room string, now time.Time) []store.PresenceRow {
	return pp.rooms.get(room, func() ([]store.PresenceRow, error) {
		ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
		defer cancel()
		rows, err := pp.st.Presence(ctx, room, now.UnixMilli())
		if err != nil {
			pp.log.Warn("presence_read_failed", zap.String("room", room), zap.Error(err))
		}
		return rows, err
	})
}

// read returns the presence of room, of every room for "", at now.
func (pp *people) read(room string, now time.Time) map[string]map[int64]*person {
	return peopleOf(pp.rows(room, now), now)
}

// open records a stream of tab and returns its release. The stream renews the tab every half grace
// until then, on the open's clock moved by the time passed. A stream with no tab id gets a random
// one: pods share the ids.
func (pp *people) open(room string, pid int64, id string, now time.Time) (release func(time.Time)) {
	if !tabID.MatchString(id) {
		id = "stream-" + NewToken()[:10]
	}
	ctx, stop := context.WithCancel(context.Background())
	at := pp.times(now)
	renew := func(now time.Time) {
		at.Now = now.UnixMilli()
		if err := pp.write(ctx, room, func(ctx context.Context) error { return pp.st.PresenceOpen(ctx, room, pid, id, at) }); ctx.Err() == nil {
			pp.warn(room, err)
		}
	}
	renew(now)
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Duration(at.Grace) * time.Millisecond / 2)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				renew(now.Add(time.Since(start)))
			}
		}
	}()
	return func(at time.Time) {
		stop()
		<-done
		pp.warn(room, pp.write(context.Background(), room, func(ctx context.Context) error { return pp.st.PresenceRelease(ctx, room, pid, id, pp.times(at)) }))
	}
}

// beat is a tab's heartbeat. gone drops the tab at once, as its page closes: its audio state goes
// with it, while the participant stays here for the grace, in case it was a reload. False when the
// participant holds maxTabs already.
func (pp *people) beat(room string, pid int64, id string, st tab, gone bool, now time.Time) (bool, error) {
	ok := false
	err := pp.write(context.Background(), room, func(ctx context.Context) error {
		var err error
		ok, err = pp.st.PresenceBeat(ctx, room, pid, id, store.TabState{Audio: st.audio, Mic: st.mic, Muted: st.muted, Deaf: st.deaf, Focused: st.focused},
			gone, maxTabs, pp.times(now))
		return err
	})
	return ok, err
}

// deviceBeat is a tab's device transcription beat: on holds the participant's flag for deviceLapse,
// off drops the tab's at once (010-AC13). The tab's presence and audio state stay as they are.
// False when the participant holds maxTabs tabs already.
func (pp *people) deviceBeat(room string, pid int64, id string, on bool, now time.Time) (bool, error) {
	ok := false
	err := pp.write(context.Background(), room, func(ctx context.Context) error {
		var err error
		ok, err = pp.st.PresenceDevice(ctx, room, pid, id, on, maxTabs, pp.times(now))
		return err
	})
	return ok, err
}

// deviceFlags lists, for each of rooms with any, the participants transcribing on their device at
// now, sorted (010-AC14).
func (pp *people) deviceFlags(rooms []string, now time.Time) map[string][]int64 {
	out := map[string][]int64{}
	if len(rooms) == 0 {
		return out
	}
	all := pp.read("", now)
	for _, room := range rooms {
		if _, done := out[room]; done {
			continue
		}
		var ids []int64
		for pid, p := range all[room] {
			if p.device {
				ids = append(ids, pid)
			}
		}
		if len(ids) > 0 {
			slices.Sort(ids)
			out[room] = ids
		}
	}
	return out
}

// deviceCount is how many participants transcribe on their device at now, over every room.
func (pp *people) deviceCount(now time.Time) int {
	n := 0
	for _, r := range pp.read("", now) {
		for _, p := range r {
			if p.device {
				n++
			}
		}
	}
	return n
}

// focused reports whether one of participant pid's live tabs in room has the focus.
func (pp *people) focused(room string, pid int64, now time.Time) bool {
	p := pp.read(room, now)[room][pid]
	return p != nil && slices.ContainsFunc(p.tabs, func(t tab) bool { return t.focused && t.alive(now) })
}

// here reports participant pid of room present at now.
func (pp *people) here(room string, pid int64, now time.Time) bool {
	p := pp.read(room, now)[room][pid]
	return p != nil && p.alive(now)
}

// forget drops the pod's copies holding a deleted room; its rows went with it.
func (pp *people) forget(room string) {
	pp.rooms.drop(room)
	pp.rooms.drop("")
	pp.hands.drop(room)
}

// hand raises or lowers pid's hand in room at now, then drops the pod's copy of the room's hands.
func (pp *people) hand(room string, pid int64, up bool, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
	defer cancel()
	var err error
	if up {
		err = pp.st.RaiseHand(ctx, room, pid, now.UnixMilli())
	} else {
		err = pp.st.LowerHand(ctx, room, pid)
	}
	pp.hands.drop(room)
	return err
}

// queueOf is room's raised hands at now, earliest first (031-AC2), from room's people ps: a hand
// raised before its participant's current stay, or whose participant is gone past the grace,
// lapsed (031-AC6).
func (pp *people) queueOf(room string, ps map[int64]*person, now time.Time) []int64 {
	hs := pp.hands.get(room, func() ([]store.Hand, error) {
		ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
		defer cancel()
		hs, err := pp.st.Hands(ctx, room, now.UnixMilli(), presenceGrace.Milliseconds())
		if err != nil {
			pp.log.Warn("hands_read_failed", zap.String("room", room), zap.Error(err))
		}
		return hs, err
	})
	var out []int64
	for _, h := range hs {
		if p := ps[h.ParticipantID]; p != nil && p.alive(now) && h.RaisedMS >= p.since.UnixMilli() {
			out = append(out, h.ParticipantID)
		}
	}
	return out
}

// presenceOf is one participant in a "presence" event.
type presenceOf struct {
	ID   int64 `json:"id"`
	Here bool  `json:"here"`
	// Audio is true while one of their tabs is in the room's audio, Mic while it publishes their
	// live microphone, Muted while its microphone is published but muted, Deaf while it hears
	// nobody (005-AC22). In the audio with neither Mic nor Muted is listening only.
	Audio bool `json:"audio"`
	Mic   bool `json:"mic"`
	Muted bool `json:"muted"`
	Deaf  bool `json:"deaf"`
	// SeenAt is when a participant who left was last here; nil while here.
	SeenAt *time.Time `json:"seen_at,omitempty"`
	// HereSince is when a participant here arrived; nil while away (009-AC36).
	HereSince *time.Time `json:"here_since,omitempty"`
	// DeviceSTT is true while their voice is transcribed on their own device (010-AC13).
	DeviceSTT bool `json:"device_stt"`
	// Hand is their rank in the room's hand queue from 1, 0 with no hand raised (031-AC4).
	Hand int `json:"hand,omitempty"`
}

// snapshot is room's presence at now, by participant id, from the pod's copy of the room's rows.
func (pp *people) snapshot(room string, now time.Time) []presenceOf {
	out := []presenceOf{}
	ps := pp.read(room, now)[room]
	for pid, p := range ps {
		v := presenceOf{ID: pid, Here: p.alive(now), DeviceSTT: p.device}
		switch {
		case v.Here:
			if since := p.since; !since.IsZero() {
				v.HereSince = &since
			}
			for _, t := range p.tabs {
				if t.alive(now) {
					v.Audio, v.Mic, v.Muted, v.Deaf = v.Audio || t.audio, v.Mic || t.mic, v.Muted || t.muted, v.Deaf || t.deaf
				}
			}
			v.Muted = v.Muted && !v.Mic
		case now.Sub(p.last) > peopleForget && !v.DeviceSTT:
			continue
		case !p.last.IsZero():
			last := p.last
			v.SeenAt = &last
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b presenceOf) int { return cmp.Compare(a.ID, b.ID) })
	for rank, pid := range pp.queueOf(room, ps, now) {
		if i := slices.IndexFunc(out, func(v presenceOf) bool { return v.ID == pid }); i >= 0 {
			out[i].Hand = rank + 1
		}
	}
	return out
}

func samePresence(a, b presenceOf) bool {
	return a.ID == b.ID && a.Here == b.Here && a.Audio == b.Audio && a.Mic == b.Mic && a.Muted == b.Muted && a.Deaf == b.Deaf &&
		a.DeviceSTT == b.DeviceSTT && a.Hand == b.Hand && sameTime(a.SeenAt, b.SeenAt) && sameTime(a.HereSince, b.HereSince)
}

func sameTime(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

// presenceEvent renders the "presence" SSE event.
func presenceEvent(ps []presenceOf) string {
	b, _ := json.Marshal(map[string]any{"people": ps}) // plain values cannot fail to marshal
	return "event: presence\ndata: " + string(b) + "\n\n"
}

// postPresence is a tab's heartbeat: it keeps its participant here and reports its audio state.
// The page sends one with gone set as it closes (005-AC6). A beat with device_stt is the device
// transcription beat of the tab instead, sent by its worker (010-AC13).
func (s *Server) postPresence(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Tab   string `json:"tab"`
		Audio bool   `json:"audio"`
		Mic   bool   `json:"mic"`
		Muted bool   `json:"muted"`
		Deaf  bool   `json:"deaf"`
		Gone  bool   `json:"gone"`
		// Focused: the page is visible and has the focus (005-AC24).
		Focused   bool  `json:"focused"`
		DeviceSTT *bool `json:"device_stt"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !tabID.MatchString(req.Tab) {
		httpError(w, http.StatusBadRequest, "tab must be 1 to 64 letters, digits, - or _")
		return
	}
	if req.DeviceSTT != nil {
		// Without the offer no page transcribes: a flag would make the worker skip a voice nobody hears.
		if s.device == nil {
			httpError(w, http.StatusNotFound, "device transcription is not offered on this server")
			return
		}
		// A tab that has not caught up would show their voice as transcribed on their device (010-AC23).
		if *req.DeviceSTT && sttOff(w, sess.p) {
			return
		}
		ok, err := s.people.deviceBeat(sess.p.RoomID, sess.p.ID, req.Tab, *req.DeviceSTT, s.lease())
		if err != nil {
			s.serverError(w, "presence_failed", err)
			return
		}
		if !ok {
			httpError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d open tabs", maxTabs))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	st := tab{audio: req.Audio, mic: req.Audio && req.Mic, muted: req.Audio && req.Muted && !req.Mic, deaf: req.Audio && req.Deaf, focused: req.Focused}
	ok, err := s.people.beat(sess.p.RoomID, sess.p.ID, req.Tab, st, req.Gone, s.lease())
	if err != nil {
		s.serverError(w, "presence_failed", err)
		return
	}
	if !ok {
		httpError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d open tabs", maxTabs))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// anyoneHere reports a participant of room present at now (006-AC18).
func (pp *people) anyoneHere(room string, now time.Time) bool {
	for _, p := range pp.read(room, now)[room] {
		if p.alive(now) {
			return true
		}
	}
	return false
}

// present is the rooms with a participant here at now.
func (pp *people) present(now time.Time) map[string]bool {
	out := map[string]bool{}
	for room, ps := range pp.read("", now) {
		for _, p := range ps {
			if p.alive(now) {
				out[room] = true
				break
			}
		}
	}
	return out
}

// listening is the rooms where a participant here hears the call at now: a tab of theirs is in the
// audio and not deafened (013-AC1).
func (pp *people) listening(now time.Time) map[string]bool {
	out := map[string]bool{}
	for room, ps := range pp.read("", now) {
		for _, p := range ps {
			if p.alive(now) && slices.ContainsFunc(p.tabs, func(t tab) bool { return t.alive(now) && t.audio && !t.deaf }) {
				out[room] = true
				break
			}
		}
	}
	return out
}

// putHand raises (PUT) or lowers (DELETE) the caller's hand; both are idempotent (031-AC1). No
// message, audit entry or export carries it (031-AC7).
func (s *Server) putHand(w http.ResponseWriter, r *http.Request, sess session) {
	if err := s.people.hand(sess.p.RoomID, sess.p.ID, r.Method == http.MethodPut, s.lease()); err != nil {
		s.serverError(w, "hand_failed", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lowerHand lowers a participant's hand: a room admin lowers anyone's, others their own only (031-AC1).
func (s *Server) lowerHand(w http.ResponseWriter, r *http.Request, sess session) {
	target, err := strconv.ParseInt(r.PathValue("participant"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such participant in this room")
		return
	}
	if target != sess.p.ID && !s.asRoomAdmin(w, r, &sess) {
		return
	}
	if err := s.people.hand(sess.p.RoomID, target, false, s.lease()); err != nil {
		s.serverError(w, "hand_failed", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
