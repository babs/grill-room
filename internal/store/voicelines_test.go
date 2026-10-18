package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

// 010-AC16, 017-AC15: a voice line the other source said within the window is a repeat; the same
// source saying it again, or the other source past the window, is a new line, stamped with the
// caller's time.
func TestPostVoiceLine(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := seed(t, s, "a")
	const window = 5 * time.Second
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var seen [][]string
	post := func(source string, at time.Time) bool {
		t.Helper()
		m := &Message{RoomID: "a", Kind: KindVoice, ParticipantID: p.ID, Sender: p.Name, Body: "hello there", Source: source}
		ok, err := s.PostVoiceLine(ctx, m, at, window, func(other []string) bool {
			seen = append(seen, other)
			return slices.Contains(other, "hello there")
		})
		if err != nil {
			t.Fatal(err)
		}
		if ok && !m.TS.Equal(at) {
			t.Errorf("stored at %v, want %v", m.TS, at)
		}
		return ok
	}
	if !post(VoiceSourceDevice, t0) {
		t.Fatal("first line not stored")
	}
	if post(VoiceSourceServer, t0.Add(time.Second)) {
		t.Error("the other source within the window: stored, want deduped")
	}
	if !post(VoiceSourceDevice, t0.Add(2*time.Second)) {
		t.Error("the same source again: deduped, want stored")
	}
	if !post(VoiceSourceServer, t0.Add(2*time.Second+window+time.Millisecond)) {
		t.Error("the other source past the window: deduped, want stored")
	}
	if len(seen) != 1 || !slices.Equal(seen[0], []string{"hello there"}) {
		t.Errorf("dup saw %q, want the device line once", seen)
	}
}
