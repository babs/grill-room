package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// legacyState writes a single-room daemon's state dir: rooms on two servers, an unparsable entry,
// a saved cap and allowlist, and a cached workspace for one room.
func legacyState(t *testing.T, dir string) (a1, a2, b1 Room) {
	t.Helper()
	a1, a2 = Room{Base: "https://a.example", ID: "one"}, Room{Base: "https://a.example", ID: "two"}
	b1 = Room{Base: "http://127.0.0.1:7680", ID: "local"}
	writeJSONFile(t, filepath.Join(dir, "tokens.json"), map[string]string{a1.String(): "tok-a1", a2.String(): "tok-a2", b1.String(): "tok-b1", "not a url": "x"})
	writeJSONFile(t, filepath.Join(dir, "settings.json"), Settings{MaxModel: "sonnet"})
	writeJSONFile(t, filepath.Join(dir, "allowlist.json"), []string{"pypi.org"})
	cached := (&wsCache{dir: filepath.Join(dir, "workspace-cache")}).roomDir(a1)
	if err := os.MkdirAll(cached, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cached, "deadbeef"), []byte("cached file"), 0o600); err != nil {
		t.Fatal(err)
	}
	return a1, a2, b1
}

// checkMigrated asserts the new layout under p holds the legacy state, per instance.
func checkMigrated(t *testing.T, p Paths, account string, a1, a2, b1 Room) {
	t.Helper()
	ida, _ := InstanceID(a1.Base)
	idb, _ := InstanceID(b1.Base)
	insts, errs := LoadInstances(p)
	if len(errs) != 0 || len(insts) != 2 {
		t.Fatalf("instances %+v %v", insts, errs)
	}
	for _, in := range insts {
		switch in.ID {
		case ida:
			if in.URL != a1.Base || !slices.Equal(in.Rooms, []string{a1.String(), a2.String()}) || in.Account != "default" {
				t.Errorf("instance a: %+v", in)
			}
		case idb:
			if in.URL != b1.Base || !slices.Equal(in.Rooms, []string{b1.String()}) {
				t.Errorf("instance b: %+v", in)
			}
		default:
			t.Errorf("unexpected instance %+v", in)
		}
	}
	for room, want := range map[Room]string{a1: "tok-a1", a2: "tok-a2", b1: "tok-b1"} {
		id, _ := InstanceID(room.Base)
		if got, err := NewTokens(p.InstanceState(id)).Get(room); err != nil || got != want {
			t.Errorf("token of %s: %q %v", room, got, err)
		}
	}
	if got, _ := NewTokens(p.InstanceState(idb)).Get(a1); got != "" {
		t.Error("a token landed in another instance")
	}
	for _, id := range []string{ida, idb} {
		if s, err := LoadSettings(filepath.Join(p.InstanceConfig(id), "settings.json"), Settings{}); err != nil || s.MaxModel != "sonnet" {
			t.Errorf("settings of %s: %+v %v", id, s, err)
		}
		if l, err := LoadAllowlist(filepath.Join(p.InstanceConfig(id), "allowlist.json")); err != nil || !slices.Equal(l, []string{"pypi.org"}) {
			t.Errorf("allowlist of %s: %v %v", id, l, err)
		}
	}
	cached := filepath.Join((&wsCache{dir: filepath.Join(p.InstanceCache(ida), "workspace-cache")}).roomDir(a1), "deadbeef")
	if got := readFile(t, cached); got != "cached file" {
		t.Errorf("cache: %q", got)
	}
	var acc struct{ Accounts []Account }
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(p.Config, "accounts.json"))), &acc); err != nil ||
		!slices.Equal(acc.Accounts, []Account{{Name: "default", Kind: "config_dir", ConfigDir: account}}) {
		t.Errorf("accounts: %+v %v", acc, err)
	}
	for _, f := range []string{filepath.Join(p.InstanceState(ida), "tokens.json"), filepath.Join(p.InstanceConfig(ida), "settings.json")} {
		assertPerm(t, f, 0o600)
	}
}

// 003-AC10: the default layout. The single-room daemon's dir moves into each instance, then stays
// whole as legacy-backup-<date> in the new config root; a second start finds nothing to migrate.
func TestMigrateDefaultLayout(t *testing.T) {
	home := t.TempDir()
	p, legacy, err := ResolvePaths("linux", func(string) string { return "" }, home)
	if err != nil {
		t.Fatal(err)
	}
	a1, a2, b1 := legacyState(t, legacy)
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)
	backup, err := Migrate(p, legacy, now, "/home/al/.claude-work", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if backup != filepath.Join(p.Config, "legacy-backup-2026-10-12") {
		t.Fatalf("backup %q", backup)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy dir still there: %v", err)
	}
	if got := readFile(t, filepath.Join(backup, "tokens.json")); got == "" {
		t.Fatal("backup lacks the tokens")
	}
	abs, _ := filepath.Abs("/home/al/.claude-work")
	checkMigrated(t, p, abs, a1, a2, b1)
	if backup, err := Migrate(p, legacy, now, "", zap.NewNop()); err != nil || backup != "" {
		t.Fatalf("second run: %q %v", backup, err)
	}
}

// 003-AC10: GRILL_ROOM_DAEMON_STATE_DIR. The dir stays the root; only its old entries move into
// legacy-backup-<date>, beside the new layout. A second backup on the same day gets its own name.
func TestMigrateStateDirOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	p, legacy, _ := ResolvePaths("linux", func(k string) string {
		if k == "GRILL_ROOM_DAEMON_STATE_DIR" {
			return dir
		}
		return ""
	}, "")
	a1, a2, b1 := legacyState(t, legacy)
	if err := os.MkdirAll(filepath.Join(dir, "legacy-backup-2026-10-12"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A relative CLAUDE_CONFIG_DIR is recorded absolute.
	t.Chdir(t.TempDir())
	backup, err := Migrate(p, legacy, time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local), "claude-rel", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if backup != filepath.Join(dir, "legacy-backup-2026-10-12-2") {
		t.Fatalf("backup %q", backup)
	}
	for _, n := range legacyEntries {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s still at the root: %v", n, err)
		}
		if _, err := os.Stat(filepath.Join(backup, n)); err != nil {
			t.Errorf("%s not in the backup: %v", n, err)
		}
	}
	wd, _ := os.Getwd()
	checkMigrated(t, p, filepath.Join(wd, "claude-rel"), a1, a2, b1)
}

// A migration cut short runs again and keeps what the new layout already holds: a newer token, the
// instance's rooms, its own settings.
func TestMigrateKeepsNewState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	p := Paths{Config: dir, State: dir, Cache: dir}
	a1, _, _ := legacyState(t, dir)
	ida, _ := InstanceID(a1.Base)
	newer := Room{Base: a1.Base, ID: "newer"}
	if err := saveInstance(p, Instance{ID: ida, URL: a1.Base, Rooms: []string{newer.String()}, Account: "work"}); err != nil {
		t.Fatal(err)
	}
	if err := NewTokens(p.InstanceState(ida)).Set(a1, "tok-a1-repaired"); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(p.InstanceConfig(ida), "settings.json"), Settings{MaxModel: "haiku"})
	if _, err := Migrate(p, dir, time.Now(), "", zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	in, err := loadInstance(p.InstanceConfig(ida))
	if err != nil || in.Account != "work" || in.Rooms[0] != newer.String() || len(in.Rooms) != 3 {
		t.Fatalf("instance %+v %v", in, err)
	}
	if tok, _ := NewTokens(p.InstanceState(ida)).Get(a1); tok != "tok-a1-repaired" {
		t.Fatalf("token %q", tok)
	}
	if s, _ := LoadSettings(filepath.Join(p.InstanceConfig(ida), "settings.json"), Settings{}); s.MaxModel != "haiku" {
		t.Fatalf("settings %+v", s)
	}
}

func TestMigrateNothingOrBroken(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Config: dir, State: dir, Cache: dir}
	if backup, err := Migrate(p, filepath.Join(dir, "absent"), time.Now(), "", zap.NewNop()); err != nil || backup != "" {
		t.Fatalf("no legacy state: %q %v", backup, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tokens.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(p, dir, time.Now(), "", zap.NewNop()); err == nil {
		t.Fatal("broken tokens migrated")
	}
	if _, err := os.Stat(filepath.Join(dir, "tokens.json")); err != nil {
		t.Fatalf("broken tokens not kept in place: %v", err)
	}
}

// A migration that fails to keep the old dir as its backup leaves it in place; once the cause is
// fixed, the next start finishes the job without doubling anything.
func TestMigrateCutShort(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions that bind the user")
	}
	home := t.TempDir()
	p, legacy, _ := ResolvePaths("linux", func(string) string { return "" }, home)
	a1, a2, b1 := legacyState(t, legacy)
	if err := os.MkdirAll(p.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(legacy)
	if err := os.Chmod(parent, 0o500); err != nil { //nolint:gosec // a read-only dir for the test
		t.Fatal(err)
	}
	_, err := Migrate(p, legacy, time.Now(), "/home/al/.claude-work", zap.NewNop())
	_ = os.Chmod(parent, 0o700) //nolint:gosec // restored
	if err == nil || !strings.Contains(err.Error(), "keep the old state as") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "tokens.json")); err != nil {
		t.Fatalf("legacy tokens gone: %v", err)
	}
	backup, err := Migrate(p, legacy, time.Now(), "/home/al/.claude-work", zap.NewNop())
	if err != nil || backup == "" {
		t.Fatalf("second run: %q %v", backup, err)
	}
	abs, _ := filepath.Abs("/home/al/.claude-work")
	checkMigrated(t, p, abs, a1, a2, b1)
}
