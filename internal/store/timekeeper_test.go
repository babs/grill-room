package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keepers lists the names of room's personas with the time keeper role.
func keepers(t *testing.T, s *Store, room string) []string {
	t.Helper()
	ps, err := s.RoomPersonas(t.Context(), room)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range ps {
		if p.TimeKeeper {
			out = append(out, p.Name)
		}
	}
	return out
}

// 032-AC1: one time keeper per room. Taking the role takes it from the previous one in the same
// change, and the chat says so, as for the notes watcher; the database refuses a second one and a
// wait outside the choices.
func TestOneTimeKeeper(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, grill := personaRoom(t, s, "k")
	sec, _, err := s.AddPersona(ctx, "k", templateByPreset(t, s, "secretary").ID, alice, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   int64
		on   *bool
		cut  *int
		want string
		keep []string
	}{
		{grill.ID, yes(), nil, "Alice changed 🔥 Grill: now keeps time.", []string{"Grill"}},
		{sec.ID, yes(), ptr(30), "Alice changed 📝 Secretary: now keeps time (was 🔥 Grill), speak over after off → 30 s.", []string{"Secretary"}},
		{sec.ID, nil, ptr(0), "Alice changed 📝 Secretary: speak over after 30 s → off.", []string{"Secretary"}},
		{sec.ID, no(), nil, "Alice changed 📝 Secretary: no longer keeps time.", nil},
	} {
		_, m, _, err := s.EditPersona(ctx, "k", c.id, PersonaPatch{TimeKeeper: c.on, CutInS: c.cut}, alice)
		if err != nil || m == nil || m.Body != c.want {
			t.Fatalf("edit: %+v %v, want %q", m, err, c.want)
		}
		if got := keepers(t, s, "k"); strings.Join(got, ",") != strings.Join(c.keep, ",") {
			t.Fatalf("after %q: keepers %v", c.want, got)
		}
	}
	if _, m, _, err := s.EditPersona(ctx, "k", sec.ID, PersonaPatch{TimeKeeper: no()}, alice); err != nil || m != nil {
		t.Fatalf("no-op: %+v %v", m, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE room_personas SET time_keeper = 1 WHERE room_id = 'k'`); err == nil {
		t.Fatal("two time keepers in one room")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE room_personas SET cut_in_s = 15 WHERE id = ?`, grill.ID); err == nil {
		t.Fatal("cut_in_s 15 stored")
	}
}

// 032-AC6: the Timekeeper template's copy comes with its room settings and takes the role from the
// persona that had it; any other template's copy gets none. A new version of the preset is offered,
// and taking it leaves the settings as the room has them.
func TestTimekeeperPreset(t *testing.T) {
	path := testDSN(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	alice, _, grill := personaRoom(t, s, "p")
	if _, _, _, err := s.EditPersona(ctx, "p", grill.ID, PersonaPatch{TimeKeeper: yes()}, alice); err != nil {
		t.Fatal(err)
	}
	tk, m, err := s.AddPersona(ctx, "p", templateByPreset(t, s, "timekeeper").ID, alice, true)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Badge != "⏱️" || tk.Model != "sonnet" || tk.Effort != "low" || tk.EveryMinutes != 5 || !tk.Speaks || !tk.TimeKeeper || tk.CutInS != 20 || tk.NextAt == nil {
		t.Fatalf("copy %+v", tk)
	}
	if want := `Alice added the persona ⏱️ Timekeeper, from the template "Timekeeper" v1, every N minutes off → 5, now keeps time (was 🔥 Grill), speak over after off → 20 s.`; m.Body != want {
		t.Fatalf("line %q, want %q", m.Body, want)
	}
	if got := keepers(t, s, "p"); len(got) != 1 || got[0] != "Timekeeper" {
		t.Fatalf("keepers %v", got)
	}
	sec, m, err := s.AddPersona(ctx, "p", templateByPreset(t, s, "secretary").ID, alice, true)
	if err != nil || sec.EveryMinutes != 0 || sec.TimeKeeper || sec.CutInS != 0 || m.Body != `Alice added the persona 📝 Secretary, from the template "Secretary" v1.` {
		t.Fatalf("secretary %+v %+v %v", sec, m, err)
	}
	// 032-AC6: an add held for the host never brings the role, whether another persona keeps time
	// or none does; the wait comes with it.
	for _, keeper := range []bool{true, false} {
		room := map[bool]string{true: "hk", false: "hn"}[keeper]
		host, bob, g, _ := approvingRoom(t, s, room)
		if keeper {
			if _, _, held, err := s.EditPersona(ctx, room, g.ID, PersonaPatch{TimeKeeper: yes()}, host); err != nil || held {
				t.Fatalf("host's role: %v %v", held, err)
			}
		}
		cp, m, err := s.AddPersona(ctx, room, templateByPreset(t, s, "timekeeper").ID, bob, true)
		if err != nil || !cp.Pending || cp.TimeKeeper || cp.CutInS != 20 || strings.Contains(m.Body, "keeps time") {
			t.Fatalf("held add (keeper %v): %+v %+v %v", keeper, cp, m, err)
		}
		want := map[bool]string{true: "Grill", false: ""}[keeper]
		if got := strings.Join(keepers(t, s, room), ","); got != want {
			t.Fatalf("held add (keeper %v): keepers %q", keeper, got)
		}
	}
	// The room admin's own choices, then a new preset version.
	if _, _, _, err := s.EditPersona(ctx, "p", tk.ID, PersonaPatch{EveryMinutes: ptr(10), CutInS: ptr(60)}, alice); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	i := len(presets) - 1
	old := presets[i].f.Text
	t.Cleanup(func() { presets[i].f.Text = old })
	presets[i].f.Text = old + "\nBe brief."
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.RoomPersona(ctx, "p", tk.ID)
	if err != nil || got.UpdateVersion != 2 {
		t.Fatalf("offer: %+v %v", got, err)
	}
	if got, _, _, err = s.UpdatePersona(ctx, "p", tk.ID, alice); err != nil {
		t.Fatal(err)
	}
	if got.Text != presets[i].f.Text || got.EveryMinutes != 10 || !got.Speaks || !got.TimeKeeper || got.CutInS != 60 {
		t.Fatalf("after the update %+v", got)
	}
}

// 032-AC3: a time keeper's turn with no synthesis posts nothing and leaves it "silent" until its
// next turn starts; its timer restarts from the turn's start and it counts for "every N rounds".
// Another persona's empty turn is not silent.
func TestSilentKeeperTurn(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, grill := personaRoom(t, s, "s")
	sec, _, err := s.AddPersona(ctx, "s", templateByPreset(t, s, "secretary").ID, alice, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.EditPersona(ctx, "s", sec.ID, PersonaPatch{EveryN: ptr(1)}, alice); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.EditPersona(ctx, "s", grill.ID, PersonaPatch{TimeKeeper: yes(), EveryMinutes: ptr(5)}, alice); err != nil {
		t.Fatal(err)
	}
	dm, err := s.PairDaemon(ctx, "s", alice.ID, "dmn-s")
	if err != nil {
		t.Fatal(err)
	}
	run := func(p int64, synthesis string) *Turn {
		t.Helper()
		tn := &Turn{RoomID: "s", DaemonID: dm.ID, Prompt: "go", PersonaID: p, PersonaName: "x"}
		if err := s.StartTurn(ctx, tn, false); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteTurn(ctx, "s", tn.ID, dm.ID, &TurnOutcome{SessionID: fmt.Sprintf("00000000-0000-4000-8000-%012d", tn.ID),
			Session: []byte("x\n"), Synthesis: synthesis}); err != nil {
			t.Fatal(err)
		}
		return tn
	}
	agentLines := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE room_id = 's' AND kind = 'agent'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tn := run(grill.ID, "  \n")
	p, err := s.RoomPersona(ctx, "s", grill.ID)
	if err != nil || !p.Silent || agentLines() != 0 {
		t.Fatalf("silent turn: %+v %v, %d agent lines", p, err, agentLines())
	}
	if want := tn.StartedAt.Add(5 * time.Minute).UnixMilli(); p.NextAt == nil || p.NextAt.UnixMilli() != want {
		t.Fatalf("timer %v, want from the turn's start %v", p.NextAt, tn.StartedAt)
	}
	if due, err := s.DuePersonas(ctx, "s"); err != nil || len(due) != 1 || due[0].ID != sec.ID {
		t.Fatalf("every N rounds after the silent turn: %+v %v", due, err)
	}
	run(sec.ID, "")
	if p, _ := s.RoomPersona(ctx, "s", sec.ID); p.Silent {
		t.Fatal("a persona without the role is silent")
	}
	// Its next turn clears the state, even before it ends.
	next := &Turn{RoomID: "s", DaemonID: dm.ID, Prompt: "go", PersonaID: grill.ID, PersonaName: "Grill"}
	if err := s.StartTurn(ctx, next, false); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.RoomPersona(ctx, "s", grill.ID); p.Silent {
		t.Fatal("still silent while its next turn runs")
	}
}

// 032-AC4: the export carries the role and the wait, and the copy has them; an archive without the
// fields imports with neither, and two time keepers or a wait outside the choices refuse it.
func TestExportTimeKeeper(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	richRoom(t, s, "orig")
	ps, _ := s.RoomPersonas(ctx, "orig")
	if _, err := s.db.ExecContext(ctx, `UPDATE room_personas SET time_keeper = 1, cut_in_s = 30 WHERE id = ?`, ps[0].ID); err != nil {
		t.Fatal(err)
	}
	dir := export(t, s, "orig")
	if pj := personaJSON(t, dir); !strings.Contains(pj, `"time_keeper":true,"cut_in_s":30`) {
		t.Fatalf("exported persona %s", pj)
	}
	if _, err := s.ImportRoom(ctx, dir, importOpts("copy")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RoomPersonas(ctx, "copy"); len(got) != 1 || !got[0].TimeKeeper || got[0].CutInS != 30 {
		t.Fatalf("copy %+v", got)
	}
	rewrite := func(f func(ps []any) []any) {
		t.Helper()
		p := filepath.Join(dir, fileRoom)
		var room map[string]any
		if err := json.Unmarshal(mustRead(t, p), &room); err != nil {
			t.Fatal(err)
		}
		room["personas"] = f(room["personas"].([]any))
		b, err := json.Marshal(room)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(func(ps []any) []any {
		delete(ps[0].(map[string]any), "time_keeper")
		delete(ps[0].(map[string]any), "cut_in_s")
		return ps
	})
	if _, err := s.ImportRoom(ctx, dir, importOpts("old")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RoomPersonas(ctx, "old"); len(got) != 1 || got[0].TimeKeeper || got[0].CutInS != 0 {
		t.Fatalf("archive from before 032: %+v", got)
	}
	for name, f := range map[string]func(ps []any) []any{
		"keeps time already": func(ps []any) []any {
			a := ps[0].(map[string]any)
			a["time_keeper"], a["cut_in_s"] = true, 0
			b := map[string]any{}
			for k, v := range a {
				b[k] = v
			}
			b["ref"], b["name"], b["resume"], b["watches_notes"] = 999999, "Other", nil, false
			return append(ps, b)
		},
		"cut_in_s must be one of": func(ps []any) []any { ps[0].(map[string]any)["cut_in_s"] = 15; return ps[:1] },
	} {
		rewrite(f)
		if _, err := s.ImportRoom(ctx, dir, importOpts("bad")); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
