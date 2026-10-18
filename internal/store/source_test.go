package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 010-AC11: an export carries each voice line's source and an import keeps it; an archive from
// before 010 has voice lines without one, which come in as the server's; an unknown source, or one
// on a line that is not a voice line, is refused.
func TestExportImportVoiceSource(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _ := richRoom(t, s, "orig")
	for _, src := range []string{VoiceSourceDevice, VoiceSourceServer} {
		m := &Message{RoomID: "orig", Kind: KindVoice, ParticipantID: alice.ID, Sender: alice.Name, Body: "said on " + src, Source: src}
		if err := s.InsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Message(ctx, "orig", m.ID); err != nil || got.Source != src {
			t.Fatalf("stored %+v %v, want source %s", got, err, src)
		}
	}
	// The store refuses a voice line without a source, and a source on any other line.
	for _, m := range []*Message{
		{RoomID: "orig", Kind: KindVoice, ParticipantID: alice.ID, Sender: alice.Name, Body: "no source"},
		{RoomID: "orig", Kind: KindText, ParticipantID: alice.ID, Sender: alice.Name, Body: "typed", Source: VoiceSourceDevice},
		{RoomID: "orig", Kind: KindSystem, Sender: "S", Body: "said", Source: VoiceSourceServer},
	} {
		if err := s.InsertMessage(ctx, m); err == nil || m.ID != 0 {
			t.Fatalf("stored %+v", m)
		}
	}
	dir := export(t, s, "orig")
	raw, err := os.ReadFile(filepath.Join(dir, fileMessages))
	if err != nil {
		t.Fatal(err)
	}
	lines := string(raw)
	if strings.Count(lines, `"source":`) != 2 || !strings.Contains(lines, `"body":"said on device","source":"device"`) ||
		!strings.Contains(lines, `"body":"said on server","source":"server"`) {
		t.Fatalf("export lines:\n%s", lines)
	}
	sources := func(room string) map[string]string {
		t.Helper()
		msgs, err := s.Messages(ctx, room, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, m := range msgs {
			out[m.Body] = m.Kind + "/" + m.Source
		}
		return out
	}
	if _, err := s.ImportRoom(ctx, dir, importOpts("copy")); err != nil {
		t.Fatal(err)
	}
	got := sources("copy")
	if got["said on device"] != "voice/device" || got["said on server"] != "voice/server" || got["hello, third time"] != "text/" {
		t.Fatalf("copy %v", got)
	}

	withLines := func(edit func(string) string) string {
		t.Helper()
		d := t.TempDir()
		if err := os.CopyFS(d, os.DirFS(dir)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, fileMessages), []byte(edit(lines)), 0o600); err != nil {
			t.Fatal(err)
		}
		return d
	}
	old := withLines(func(b string) string {
		return strings.NewReplacer(`,"source":"device"`, "", `,"source":"server"`, "").Replace(b)
	})
	if _, err := s.ImportRoom(ctx, old, importOpts("old")); err != nil {
		t.Fatal(err)
	}
	if got := sources("old"); got["said on device"] != "voice/server" || got["said on server"] != "voice/server" || got["hello, third time"] != "text/" {
		t.Fatalf("archive without sources %v", got)
	}
	for name, c := range map[string]struct{ edit func(string) string }{
		"unknown source":   {func(b string) string { return strings.Replace(b, `"source":"device"`, `"source":"phone"`, 1) }},
		"empty-ish source": {func(b string) string { return strings.Replace(b, `"source":"device"`, `"source":" device"`, 1) }},
		"source on text": {func(b string) string {
			return strings.Replace(b, `"body":"hello, third time"`, `"body":"hello, third time","source":"server"`, 1)
		}},
		"source on system": {func(b string) string {
			return strings.Replace(b, `"kind":"system",`, `"kind":"system","source":"device",`, 1)
		}},
	} {
		d := withLines(c.edit)
		if raw, _ := os.ReadFile(filepath.Join(d, fileMessages)); string(raw) == lines {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if _, err := s.ImportRoom(ctx, d, importOpts("bad")); !errors.Is(err, ErrImportRefused) || !strings.Contains(err.Error(), "source") {
			t.Errorf("%s: %v, want a refused source", name, err)
		}
	}
}
