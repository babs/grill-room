package store

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ttsLine(room string, queuedMS int64) TTSLine {
	return TTSLine{Room: room, Persona: "Grill", Voice: "alba", Rate: 1, Text: "hello", QuietMS: 1000, ResumeS: 5, QueueS: 40, QueuedAtMS: queuedMS}
}

// 013-AC14, 017-AC10: one sequence numbers the lines above the clock-based seqs of before; a line
// read is sent, a line expired unsent counts as no_service, the newest keep stand; the cursor
// resumes after the newest sent line.
func TestTTSLines(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if _, err := s.CreateRoom(ctx, "r", "R"); err != nil {
		t.Fatal(err)
	}
	if resume, last, err := s.TTSCursor(ctx); err != nil || resume != last || last < time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro() {
		t.Fatalf("empty cursor %d %d %v: resume at the sequence, seeded from the clock", resume, last, err)
	}
	now := time.Now().UnixMilli()
	first, _, err := s.AddTTSLine(ctx, ttsLine("r", now), 500, now)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.AddTTSLine(ctx, ttsLine("r", now), 500, now)
	if err != nil || second <= first {
		t.Fatalf("second %d after %d: %v", second, first, err)
	}
	if resume, _, _ := s.TTSCursor(ctx); resume != first-1 {
		t.Errorf("nothing sent: resume at %d, want %d", resume, first-1)
	}
	got, err := s.TTSLinesAfter(ctx, first, now)
	if err != nil || len(got) != 1 || got[0] != (TTSLine{Seq: second, Room: "r", Persona: "Grill", Voice: "alba", Rate: 1, Text: "hello", QuietMS: 1000,
		ResumeS: 5, QueueS: 40, QueuedAtMS: now}) {
		t.Fatalf("after %d: %+v %v", first, got, err)
	}
	if resume, _, _ := s.TTSCursor(ctx); resume != second {
		t.Errorf("resume at %d, want the sent %d", resume, second)
	}
	later := now + 41_000
	if got, _ := s.TTSLinesAfter(ctx, 0, later); len(got) != 0 {
		t.Errorf("expired lines handed out: %+v", got)
	}
	if n, err := s.SweepTTSLines(ctx, later); err != nil || n != 1 {
		t.Errorf("sweep: no_service %d, want 1 (the unsent first): %v", n, err)
	}
	if n, err := s.SweepTTSLines(ctx, later); err != nil || n != 0 {
		t.Errorf("second sweep: %d %v", n, err)
	}

	var dropped int64
	for range 8 {
		_, n, err := s.AddTTSLine(ctx, ttsLine("r", later), 5, later)
		if err != nil {
			t.Fatal(err)
		}
		dropped += n
	}
	var kept int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tts_lines`).Scan(&kept); err != nil || kept != 5 || dropped != 3 {
		t.Errorf("kept %d, dropped unsent %d: want 5 and 3 (%v)", kept, dropped, err)
	}
}

// 013-AC14, 017-AC14: the heartbeat and the stream's lease share one row; a heartbeat keeps the
// lease, a renewal never shortens it.
func TestTTSPodRow(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if p, err := s.TTSPodState(ctx); err != nil || p.BeatAtMS != 0 || p.StreamUntilMS != 0 {
		t.Fatalf("before any beat: %+v %v", p, err)
	}
	if err := s.RenewTTSStream(ctx, 2000); err != nil {
		t.Fatal(err)
	}
	if err := s.RenewTTSStream(ctx, 1500); err != nil {
		t.Fatal(err)
	}
	states := json.RawMessage(`[{"room":"r","persona_id":1,"state":"speaking"}]`)
	if err := s.BeatTTSPod(ctx, TTSPod{Pod: "tts-1", Rooms: []string{"r"}, Bots: []string{"b"}, States: states}, 1000); err != nil {
		t.Fatal(err)
	}
	p, err := s.TTSPodState(ctx)
	if err != nil || p.Pod != "tts-1" || !slices.Equal(p.Rooms, []string{"r"}) || !slices.Equal(p.Bots, []string{"b"}) ||
		string(p.States) != string(states) || p.BeatAtMS != 1000 || p.StreamUntilMS != 2000 {
		t.Fatalf("row %+v %s %v", p, p.States, err)
	}
	// 017-AC14: a stream ending releases the lease only while it still holds its last renewal.
	if err := s.ReleaseTTSStream(ctx, 1500); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.TTSPodState(ctx); p.StreamUntilMS != 2000 {
		t.Fatalf("an older stream's release took the lease: %d", p.StreamUntilMS)
	}
	if err := s.ReleaseTTSStream(ctx, 2000); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.TTSPodState(ctx); p.StreamUntilMS != 0 || p.BeatAtMS != 1000 {
		t.Fatalf("after the holder's release: %+v", p)
	}
}

// 013-AC16, 017 I21: two pods defaulting a room's voices at once hand out each voice once, and a
// stored voice never moves.
func TestDefaultVoicesOnce(t *testing.T) {
	ctx := context.Background()
	dsn := testDSN(t)
	pods := []*Store{openAt(t, dsn), openAt(t, dsn)}
	p := seed(t, pods[0], "a")
	if _, _, err := pods[0].AddPersona(ctx, "a", templateByPreset(t, pods[0], PresetDefault).ID, p); err != nil {
		t.Fatal(err)
	}
	catalogue := []string{"v1", "v2", "v3"}
	var picks atomic.Int64
	var in atomic.Int32
	allIn := make(chan struct{})
	pickFor := func() func(used map[string]bool) string {
		first := true
		return func(used map[string]bool) string {
			picks.Add(1)
			// Without the lock both pods read the room before either writes: wait a little for the other.
			if first {
				first = false
				if in.Add(1) == 2 {
					close(allIn)
				}
				select {
				case <-allIn:
				case <-time.After(500 * time.Millisecond):
				}
			}
			for _, v := range catalogue {
				if !used[v] {
					return v
				}
			}
			return catalogue[0]
		}
	}
	var wg sync.WaitGroup
	for _, s := range pods {
		pick := pickFor()
		wg.Go(func() {
			if _, err := s.DefaultVoices(ctx, "a", pick); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	ps, err := pods[1].RoomPersonas(ctx, "a")
	if err != nil || len(ps) != 2 || ps[0].Voice == "" || ps[1].Voice == "" || ps[0].Voice == ps[1].Voice || picks.Load() != 2 {
		t.Fatalf("voices %q %q after %d picks: %v", ps[0].Voice, ps[1].Voice, picks.Load(), err)
	}
	got, err := pods[1].DefaultVoices(ctx, "a", func(map[string]bool) string { return "moved" })
	if err != nil || got[ps[0].ID] != ps[0].Voice || got[ps[1].ID] != ps[1].Voice {
		t.Errorf("a stored voice moved: %v %v", got, err)
	}
}

// 013-AC34: the demand counts each room with a speaking persona once; a pending persona or a room
// not asked about counts none.
func TestSpeakingRooms(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for _, r := range []string{"a", "b", "c"} {
		p := seed(t, s, r)
		if _, _, err := s.AddPersona(ctx, r, templateByPreset(t, s, PresetDefault).ID, p); err != nil {
			t.Fatal(err)
		}
	}
	for q, args := range map[string][]any{
		`UPDATE room_personas SET speaks = 1 WHERE room_id IN ('a', 'c')`:      nil,
		`UPDATE room_personas SET speaks = 1, pending = 1 WHERE room_id = 'b'`: nil,
	} {
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		rooms []string
		want  int
	}{{nil, 0}, {[]string{"a"}, 1}, {[]string{"a", "b"}, 1}, {[]string{"a", "b", "c", "nope"}, 2}} {
		if n, err := s.SpeakingRooms(ctx, c.rooms); err != nil || n != c.want {
			t.Errorf("%v: %d %v, want %d", c.rooms, n, err, c.want)
		}
	}
}
