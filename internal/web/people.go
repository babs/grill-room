package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxTabs bounds the tabs one participant holds open; past it a new tab is refused.
	maxTabs = 20
	// peopleForget is how long a gone participant's last-seen time stays in memory; the list falls
	// back on the stored one after.
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

// tab is one open room page of a participant: its event streams, its last heartbeat, and the audio
// state it reported.
type tab struct {
	streams                 int
	beat                    time.Time
	audio, mic, muted, deaf bool
	// focused is the page in the foreground with the focus: no push for this room then (005-AC24).
	focused bool
}

// person is a participant's presence in their room: their tabs, their last sign of life, and
// since when they are here (009-AC36).
type person struct {
	tabs  map[string]*tab
	last  time.Time
	since time.Time
	// device holds, by tab id, the last device beat that said the tab transcribes on its device
	// (010-AC13). Apart from tabs: a device beat comes from the page's worker, with no stream.
	device map[string]time.Time
}

// deviceOn reports a tab of p transcribing on its device at now, and sweeps the lapsed tabs.
func (p *person) deviceOn(now time.Time) bool {
	on := false
	for id, at := range p.device {
		if now.Sub(at) < deviceLapse {
			on = true
		} else {
			delete(p.device, id)
		}
	}
	return on
}

// arrive starts p's stay at now unless p is here already. The caller holds the lock.
func (p *person) arrive(now time.Time) {
	if !p.alive(now) {
		p.since = now
	}
}

// people is the presence of every participant, by room then participant id (005-AC6). In memory:
// the room server runs one replica.
type people struct {
	mu sync.Mutex
	m  map[string]map[int64]*person
	// anon names the streams of clients that send no tab id.
	anon atomic.Int64
}

func (pp *people) person(room string, pid int64) *person {
	r := pp.m[room]
	if r == nil {
		r = map[int64]*person{}
		pp.m[room] = r
	}
	p := r[pid]
	if p == nil {
		p = &person{tabs: map[string]*tab{}, device: map[string]time.Time{}}
		r[pid] = p
	}
	return p
}

// open counts a stream of tab and returns its release. A stream with no tab id gets one of its own.
func (pp *people) open(room string, pid int64, id string, now time.Time) (release func(time.Time)) {
	if !tabID.MatchString(id) {
		id = "stream-" + strconv.FormatInt(pp.anon.Add(1), 10)
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()
	p := pp.person(room, pid)
	p.arrive(now)
	t := p.tabs[id]
	if t == nil {
		t = &tab{}
		p.tabs[id] = t
	}
	t.streams++
	t.beat, p.last = now, now
	return func(at time.Time) {
		pp.mu.Lock()
		defer pp.mu.Unlock()
		t.streams--
		t.beat, p.last = at, at
	}
}

// beat is a tab's heartbeat. gone drops the tab at once, as its page closes: its audio state goes
// with it, while the participant stays here for the grace, in case it was a reload. False when the
// participant holds maxTabs already.
func (pp *people) beat(room string, pid int64, id string, st tab, gone bool, now time.Time) bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	p := pp.person(room, pid)
	if !gone {
		p.arrive(now)
	}
	p.last = now
	t := p.tabs[id]
	if gone {
		delete(p.device, id)
		if t != nil && t.streams == 0 {
			delete(p.tabs, id)
		} else if t != nil {
			t.audio, t.mic, t.muted, t.deaf, t.focused = false, false, false, false, false
		}
		return true
	}
	if t == nil {
		if len(p.tabs) >= maxTabs {
			return false
		}
		t = &tab{}
		p.tabs[id] = t
	}
	t.beat, t.audio, t.mic, t.muted, t.deaf, t.focused = now, st.audio, st.mic, st.muted, st.deaf, st.focused
	return true
}

// deviceBeat is a tab's device transcription beat: on holds the participant's flag for deviceLapse,
// off drops the tab's at once (010-AC13). The tab's presence and audio state stay as they are.
// False when the participant holds maxTabs flagged tabs already.
func (pp *people) deviceBeat(room string, pid int64, id string, on bool, now time.Time) bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	p := pp.person(room, pid)
	if !on {
		delete(p.device, id)
		return true
	}
	if _, ok := p.device[id]; !ok {
		if p.deviceOn(now); len(p.device) >= maxTabs {
			return false
		}
	}
	p.device[id] = now
	return true
}

// deviceFlags lists, for each of rooms with any, the participants transcribing on their device at
// now, sorted (010-AC14).
func (pp *people) deviceFlags(rooms []string, now time.Time) map[string][]int64 {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	out := map[string][]int64{}
	for _, room := range rooms {
		if _, done := out[room]; done {
			continue
		}
		var ids []int64
		for pid, p := range pp.m[room] {
			if p.deviceOn(now) {
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
	pp.mu.Lock()
	defer pp.mu.Unlock()
	n := 0
	for _, r := range pp.m {
		for _, p := range r {
			if p.deviceOn(now) {
				n++
			}
		}
	}
	return n
}

// focused reports whether one of participant pid's live tabs in room has the focus.
func (pp *people) focused(room string, pid int64, now time.Time) bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	p := pp.m[room][pid]
	if p == nil {
		return false
	}
	for _, t := range p.tabs {
		if t.focused && t.alive(now) {
			return true
		}
	}
	return false
}

// here reports participant pid of room present at now.
func (pp *people) here(room string, pid int64, now time.Time) bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	p := pp.m[room][pid]
	return p != nil && p.alive(now)
}

func (t *tab) alive(now time.Time) bool { return t.streams > 0 || now.Sub(t.beat) <= presenceGrace }

func (p *person) alive(now time.Time) bool {
	if now.Sub(p.last) <= presenceGrace {
		return true
	}
	for _, t := range p.tabs {
		if t.streams > 0 {
			return true
		}
	}
	return false
}

// forget drops a deleted room's presence: no stream will sweep it again.
func (pp *people) forget(room string) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	delete(pp.m, room)
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
}

// snapshot is room's presence at now, by participant id, and sweeps the dead tabs and the people
// gone for peopleForget.
func (pp *people) snapshot(room string, now time.Time) []presenceOf {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	out := []presenceOf{}
	for pid, p := range pp.m[room] {
		for id, t := range p.tabs {
			if !t.alive(now) {
				delete(p.tabs, id)
			}
		}
		v := presenceOf{ID: pid, Here: p.alive(now), DeviceSTT: p.deviceOn(now)}
		switch {
		case v.Here:
			if since := p.since; !since.IsZero() {
				v.HereSince = &since
			}
			for _, t := range p.tabs {
				v.Audio, v.Mic, v.Muted, v.Deaf = v.Audio || t.audio, v.Mic || t.mic, v.Muted || t.muted, v.Deaf || t.deaf
			}
			v.Muted = v.Muted && !v.Mic
		case now.Sub(p.last) > peopleForget && !v.DeviceSTT:
			delete(pp.m[room], pid)
			continue
		case !p.last.IsZero():
			last := p.last
			v.SeenAt = &last
		}
		out = append(out, v)
	}
	if len(pp.m[room]) == 0 {
		delete(pp.m, room)
	}
	slices.SortFunc(out, func(a, b presenceOf) int { return int(a.ID - b.ID) })
	return out
}

func samePresence(a, b presenceOf) bool {
	return a.ID == b.ID && a.Here == b.Here && a.Audio == b.Audio && a.Mic == b.Mic && a.Muted == b.Muted && a.Deaf == b.Deaf &&
		a.DeviceSTT == b.DeviceSTT && sameTime(a.SeenAt, b.SeenAt) && sameTime(a.HereSince, b.HereSince)
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
		if !s.people.deviceBeat(sess.p.RoomID, sess.p.ID, req.Tab, *req.DeviceSTT, s.now()) {
			httpError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d open tabs", maxTabs))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	st := tab{audio: req.Audio, mic: req.Audio && req.Mic, muted: req.Audio && req.Muted && !req.Mic, deaf: req.Audio && req.Deaf, focused: req.Focused}
	if !s.people.beat(sess.p.RoomID, sess.p.ID, req.Tab, st, req.Gone, s.now()) {
		httpError(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d open tabs", maxTabs))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// anyoneHere reports a participant of room present at now (006-AC18).
func (pp *people) anyoneHere(room string, now time.Time) bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	for _, p := range pp.m[room] {
		if p.alive(now) {
			return true
		}
	}
	return false
}

// listening is the rooms where a participant here hears the call at now: a tab of theirs is in the
// audio and not deafened (013-AC1).
func (pp *people) listening(now time.Time) map[string]bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	out := map[string]bool{}
	for room, ps := range pp.m {
		for _, p := range ps {
			if !p.alive(now) {
				continue
			}
			for _, t := range p.tabs {
				if t.alive(now) && t.audio && !t.deaf {
					out[room] = true
				}
			}
		}
	}
	return out
}
