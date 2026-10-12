package daemon

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// legacyEntries are what a single-room daemon kept in its state dir.
var legacyEntries = []string{"tokens.json", "settings.json", "allowlist.json", "workspace-cache"}

// Migrate moves a single-room daemon's state from legacy into p's instance layout (003-AC10).
// Each paired room goes to the instance of its room server, with its token, a copy of the
// settings and allowlist, and its workspace cache; claudeConfigDir becomes account "default". The
// old files end up in <config>/legacy-backup-<date>, whose path it returns; "" when there was
// nothing to migrate. Every step keeps what the new layout already holds, so a migration cut
// short runs again on the next start.
func Migrate(p Paths, legacy string, now time.Time, claudeConfigDir string, log *zap.Logger) (string, error) {
	var found []string
	for _, n := range legacyEntries {
		if _, err := os.Lstat(filepath.Join(legacy, n)); err == nil {
			found = append(found, n)
		}
	}
	if len(found) == 0 {
		return "", nil
	}
	tokens, err := NewTokens(legacy).load()
	if err != nil {
		return "", fmt.Errorf("legacy tokens: %w", err)
	}
	byInstance := map[string]*Instance{}
	instTokens := map[string]map[string]string{}
	for _, u := range slices.Sorted(maps.Keys(tokens)) {
		room, err := ParseRoomURL(u)
		if err != nil {
			log.Warn("migration_room_skipped", zap.Error(err))
			continue
		}
		id, err := InstanceID(room.Base)
		if err != nil {
			log.Warn("migration_room_skipped", zap.Error(err))
			continue
		}
		in := byInstance[id]
		if in == nil {
			in = &Instance{ID: id, URL: room.Base, Account: "default"}
			byInstance[id], instTokens[id] = in, map[string]string{}
		}
		in.addRoom(room.String())
		instTokens[id][room.String()] = tokens[u]
	}
	for id, in := range byInstance {
		if err := migrateInstance(p, legacy, *in, instTokens[id], log); err != nil {
			return "", fmt.Errorf("instance %s: %w", in.URL, err)
		}
	}
	if len(byInstance) == 0 {
		log.Warn("migration_no_paired_room", zap.String("hint", "the old settings and allowlist stay in the backup only"))
	}
	if err := writeAccounts(p, claudeConfigDir); err != nil {
		return "", err
	}
	backup, err := backupLegacy(p, legacy, found, now)
	if err != nil {
		return "", err
	}
	log.Info("migration_done", zap.Int("instances", len(byInstance)), zap.String("backup", backup))
	return backup, nil
}

func migrateInstance(p Paths, legacy string, in Instance, tokens map[string]string, log *zap.Logger) error {
	if old, err := loadInstance(p.InstanceConfig(in.ID)); err == nil {
		for _, r := range in.Rooms {
			old.addRoom(r)
		}
		in.Rooms = old.Rooms
		in.Account = cmp.Or(old.Account, in.Account)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := saveInstance(p, in); err != nil {
		return err
	}
	t := NewTokens(p.InstanceState(in.ID))
	for room, tok := range tokens {
		r, _ := ParseRoomURL(room) // parsed once already
		if have, err := t.Get(r); err != nil {
			return err
		} else if have == "" {
			if err := t.Set(r, tok); err != nil {
				return err
			}
		}
	}
	for _, n := range []string{"settings.json", "allowlist.json"} {
		if err := copyIfAbsent(filepath.Join(legacy, n), filepath.Join(p.InstanceConfig(in.ID), n)); err != nil {
			return err
		}
	}
	from, to := &wsCache{dir: filepath.Join(legacy, "workspace-cache")}, &wsCache{dir: filepath.Join(p.InstanceCache(in.ID), "workspace-cache")}
	for _, room := range in.Rooms {
		r, err := ParseRoomURL(room)
		if err != nil {
			continue
		}
		src, dst := from.roomDir(r), to.roomDir(r)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("cache dir: %w", err)
		}
		// Another filesystem refuses the rename: the cache refills from the room on the next turn.
		if err := os.Rename(src, dst); err != nil {
			log.Warn("migration_cache_skipped", zap.String("room", room), zap.Error(err))
		}
	}
	return nil
}

func copyIfAbsent(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	b, err := os.ReadFile(src) //nolint:gosec // the legacy state dir
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, b)
}

func writeAccounts(p Paths, configDir string) error {
	path := filepath.Join(p.Config, "accounts.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	// A relative dir means nothing to a supervisor started from elsewhere, by autostart say.
	// An absolute dir stays as written: Claude Code names its Keychain item after the raw value.
	if configDir != "" && !filepath.IsAbs(configDir) {
		abs, err := filepath.Abs(configDir)
		if err != nil {
			return fmt.Errorf("account default: %w", err)
		}
		configDir = abs
	}
	b, err := json.MarshalIndent(map[string][]Account{"accounts": {{Name: "default", Kind: KindConfigDir, ConfigDir: configDir}}}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

// backupLegacy moves the old state out of the way. When the legacy dir is the config root
// (GRILL_ROOM_DAEMON_STATE_DIR), only its old entries move.
func backupLegacy(p Paths, legacy string, entries []string, now time.Time) (string, error) {
	base := filepath.Join(p.Config, "legacy-backup-"+now.Format(time.DateOnly))
	dest := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
			break
		}
		dest = base + "-" + strconv.Itoa(i)
	}
	if filepath.Clean(legacy) != filepath.Clean(p.Config) {
		if err := os.MkdirAll(p.Config, 0o700); err != nil {
			return "", err
		}
		if err := os.Rename(legacy, dest); err != nil {
			return "", fmt.Errorf("keep the old state as %s: %w", dest, err)
		}
		return dest, nil
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	for _, n := range entries {
		if err := os.Rename(filepath.Join(legacy, n), filepath.Join(dest, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("keep the old %s in %s: %w", n, dest, err)
		}
	}
	return dest, nil
}
