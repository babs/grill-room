package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Room creation modes (004-AC7).
const (
	CreationAnyone     = "anyone"
	CreationSuperAdmin = "super-admin"
)

// DefaultRoomsPerProfile is how many rooms a profile may have created at once (004-AC7).
const DefaultRoomsPerProfile = 10

// UnlimitedRooms is the profile limit that lets a profile create rooms without a count (004-AC7).
const UnlimitedRooms = -1

var (
	// ErrCreationRefused is returned when a profile may not create a room; the error says why.
	ErrCreationRefused = errors.New("room creation refused")
	// ErrAtQuota is the ErrCreationRefused of an instance at its storage quota (026-AC2).
	ErrAtQuota = fmt.Errorf("%w", ErrCreationRefused)
	// ErrNoProfile is returned when appointing a participant who has no profile.
	ErrNoProfile = errors.New("this participant has no profile")
	// ErrLastAdmin is returned when the last room admin steps down.
	ErrLastAdmin = errors.New("you are the room's last admin: appoint another one first")
)

// InstanceSettings are the guardrails on rooms profiles create (004-AC7).
type InstanceSettings struct {
	RoomsPerProfile int `json:"rooms_per_profile"`
	// StorageQuota is the bytes the database may hold before profiles stop creating rooms; 0 for no
	// quota.
	StorageQuota int64 `json:"storage_quota_bytes"`
	// RoomCreation is CreationAnyone or CreationSuperAdmin.
	RoomCreation string `json:"room_creation"`
	// PluginMaxMB caps a plugin snapshot, while fetching and once extracted (009-AC9).
	PluginMaxMB int `json:"plugin_max_mb,omitempty"`
	// PurgeEmptyProfiles deletes empty profiles after EmptyProfileIdle of inactivity; on by
	// default (011-AC17).
	PurgeEmptyProfiles bool `json:"purge_empty_profiles"`
}

// DefaultPluginMaxMB is the plugin snapshot cap until the super-admin sets one (009-AC9).
const DefaultPluginMaxMB = 20

const (
	settingRoomsPerProfile = "rooms_per_profile"
	settingStorageQuota    = "storage_quota_bytes"
	settingRoomCreation    = "room_creation"
	settingPluginMaxMB     = "plugin_max_mb"
	settingPurgeProfiles   = "purge_empty_profiles"
)

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *row
}

func instanceSettings(ctx context.Context, db rowQuerier) (InstanceSettings, error) {
	v := InstanceSettings{RoomsPerProfile: DefaultRoomsPerProfile, RoomCreation: CreationAnyone, PluginMaxMB: DefaultPluginMaxMB}
	var per, quota, mode, pluginMB, purge sql.NullString
	err := db.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key = ?), (SELECT value FROM settings WHERE key = ?),
		(SELECT value FROM settings WHERE key = ?), (SELECT value FROM settings WHERE key = ?), (SELECT value FROM settings WHERE key = ?)`,
		settingRoomsPerProfile, settingStorageQuota, settingRoomCreation, settingPluginMaxMB, settingPurgeProfiles).Scan(&per, &quota, &mode, &pluginMB, &purge)
	if err != nil {
		return v, fmt.Errorf("instance settings: %w", err)
	}
	if per.Valid {
		v.RoomsPerProfile, _ = strconv.Atoi(per.String)
	}
	if quota.Valid {
		v.StorageQuota, _ = strconv.ParseInt(quota.String, 10, 64)
	}
	if mode.Valid {
		v.RoomCreation = mode.String
	}
	if pluginMB.Valid {
		v.PluginMaxMB, _ = strconv.Atoi(pluginMB.String)
	}
	if v.PluginMaxMB < 1 {
		v.PluginMaxMB = DefaultPluginMaxMB
	}
	v.PurgeEmptyProfiles = purge.String != "0"
	return v, nil
}

// InstanceSettings returns the room creation guardrails, defaults for those never set.
func (s *Store) InstanceSettings(ctx context.Context) (InstanceSettings, error) {
	return instanceSettings(ctx, s.db)
}

// UpdateInstanceSettings runs patch on the stored settings and stores the result, in one
// transaction, so two requests patching different fields cannot undo each other. patch validates;
// its error is returned as is. The audit entry ctx carries gets the resulting settings.
func (s *Store) UpdateInstanceSettings(ctx context.Context, patch func(*InstanceSettings) error) (InstanceSettings, error) {
	var v InstanceSettings
	err := s.withTx(ctx, "update instance settings", func(tx *txn) error {
		var err error
		if v, err = instanceSettings(ctx, tx); err != nil {
			return err
		}
		if err := patch(&v); err != nil {
			return err
		}
		if err := writeInstanceSettings(ctx, tx, v); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", map[string]any{settingRoomsPerProfile: v.RoomsPerProfile, settingStorageQuota: v.StorageQuota,
			settingRoomCreation: v.RoomCreation, settingPluginMaxMB: v.PluginMaxMB, settingPurgeProfiles: v.PurgeEmptyProfiles})
	})
	return v, err
}

func writeInstanceSettings(ctx context.Context, tx *txn, v InstanceSettings) error {
	for k, val := range map[string]string{settingRoomsPerProfile: strconv.Itoa(v.RoomsPerProfile),
		settingStorageQuota: strconv.FormatInt(v.StorageQuota, 10), settingRoomCreation: v.RoomCreation, settingPluginMaxMB: strconv.Itoa(v.PluginMaxMB),
		settingPurgeProfiles: map[bool]string{true: "1", false: "0"}[v.PurgeEmptyProfiles]} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, val); err != nil {
			return fmt.Errorf("set instance settings: %w", err)
		}
	}
	return nil
}

// InstanceID returns this instance's id, drawn once: an import tells its own exports by it.
func (s *Store) InstanceID(ctx context.Context) (string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES('instance_id', ?) ON CONFLICT DO NOTHING`, rand.Text()); err != nil {
		return "", fmt.Errorf("instance id: %w", err)
	}
	return s.Setting(ctx, "instance_id")
}

type catalogQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *row
}

// UsedBytes is what the rows hold: text and bytea at their length, other values at their stored
// size. A delete frees its bytes at once.
func (s *Store) UsedBytes(ctx context.Context) (int64, error) { return usedBytes(ctx, s.db) }

// usedBytes sums every column of every table, one query built from the catalog.
// ponytail: reads all the data on each call (room creation, import, starter apply), O(data); keep
// a running total per table, updated by the writes, if that gets slow.
func usedBytes(ctx context.Context, db catalogQuerier) (int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.relname::text, a.attname::text, a.atttypid IN ('text'::regtype, 'bytea'::regtype)
		FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r' ORDER BY c.relname, a.attnum`)
	if err != nil {
		return 0, fmt.Errorf("used bytes: %w", err)
	}
	defer rows.Close()
	cols := map[string][]string{}
	var tables []string
	for rows.Next() {
		var table, col string
		var length bool
		if err := rows.Scan(&table, &col, &length); err != nil {
			return 0, fmt.Errorf("used bytes: %w", err)
		}
		// octet_length: a compressed or out-of-line value still counts at its size.
		f := "pg_column_size"
		if length {
			f = "octet_length"
		}
		if _, ok := cols[table]; !ok {
			tables = append(tables, table)
		}
		cols[table] = append(cols[table], "COALESCE("+f+"("+pgx.Identifier{col}.Sanitize()+"), 0)")
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("used bytes: %w", err)
	}
	if len(tables) == 0 {
		return 0, nil
	}
	sums := make([]string, len(tables))
	for i, t := range tables {
		sums[i] = "(SELECT COALESCE(SUM(" + strings.Join(cols[t], " + ") + "), 0) FROM " + pgx.Identifier{t}.Sanitize() + ")"
	}
	var n int64
	if err := db.QueryRowContext(ctx, `SELECT (`+strings.Join(sums, " + ")+`)::bigint`).Scan(&n); err != nil {
		return 0, fmt.Errorf("used bytes: %w", err)
	}
	return n, nil
}

// mb renders bytes for a refusal.
func mb(n int64) string { return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB" }

// CreateProfileRoom creates room id named name for profile profileID, which joins it as
// displayName and becomes its first room admin (004-AC7, 004-AC8). The profile joins through an
// invite of its own, inviteHash, whose token nobody holds, and gets its shareable invite, link with
// its hash linkHash (011-AC26). ErrCreationRefused, wrapped with the reason.
//
// With setup, the room holds the starter's copies instead of the Default persona (006-AC11).
func (s *Store) CreateProfileRoom(ctx context.Context, id, name string, profileID int64, displayName, inviteHash, tokenHash, link, linkHash string, setup *RoomSetup) (*Room, error) {
	var r *Room
	err := s.withTx(ctx, "create room", func(tx *txn) error {
		var err error
		r, err = createProfileRoom(ctx, tx, id, name, profileID, displayName, inviteHash, tokenHash, link, linkHash, setup)
		return err
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// CreateAnonymousRoom creates a profile with recoveryHash and sessionHash and, as CreateProfileRoom
// does, a blank room for it, in one transaction: a refusal leaves no profile behind (019-AC2,
// 019-AC5). The audit entry ctx carries gets the new profile as its actor. It returns the room and
// the profile's id.
func (s *Store) CreateAnonymousRoom(ctx context.Context, id, name, displayName, recoveryHash, sessionHash, inviteHash, tokenHash, link, linkHash string) (*Room, int64, error) {
	var r *Room
	var profileID int64
	err := s.withTx(ctx, "create anonymous room", func(tx *txn) error {
		// A new profile has created no room: its own limit cannot refuse, the instance's may.
		v, err := instanceSettings(ctx, tx)
		if err != nil {
			return err
		}
		// Again under the lock: the settings may have changed since AnonymousCreationAllowed (026-AC1).
		if err := anonymousRefusal(v); err != nil {
			return err
		}
		used, err := quotaUsed(ctx, tx, v)
		if err != nil {
			return err
		}
		if profileID, err = createProfile(ctx, tx, recoveryHash, sessionHash); err != nil {
			return err
		}
		if p, _ := ctx.Value(auditKey{}).(*pendingAudit); p != nil {
			if err := tx.QueryRowContext(ctx, `SELECT uuid FROM profiles WHERE id = ?`, profileID).Scan(&p.e.Actor); err != nil {
				return fmt.Errorf("create anonymous room: %w", err)
			}
			p.e.Actor = ActorProfile + p.e.Actor
		}
		r, err = insertProfileRoom(ctx, tx, v, used, id, name, profileID, displayName, inviteHash, tokenHash, link, linkHash, nil)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return r, profileID, nil
}

// AnonymousCreationAllowed returns ErrCreationRefused while only the super-admin creates rooms, or
// while no storage quota is set (026-AC1). It reads the settings alone, without the write lock; the
// storage quota, which scans every table, is CreateAnonymousRoom's to check (019-AC5).
func (s *Store) AnonymousCreationAllowed(ctx context.Context) error {
	v, err := instanceSettings(ctx, s.db)
	if err != nil {
		return err
	}
	return anonymousRefusal(v)
}

// errSuperAdminOnly refuses a room while only the super-admin creates them.
var errSuperAdminOnly = fmt.Errorf("%w: only the super-admin creates rooms on this instance", ErrCreationRefused)

// refusal is an ErrCreationRefused whose text is the whole reason.
type refusal string

func (r refusal) Error() string { return string(r) }
func (refusal) Unwrap() error   { return ErrCreationRefused }

// errNoQuota refuses an anonymous room without a storage quota: the per-IP limit alone does not
// bound storage (026-AC1).
const errNoQuota = refusal("Room creation without an invite needs a storage quota: set one in /admin › Server")

// anonymousRefusal returns ErrCreationRefused, with why, when settings v let no anonymous room be
// created before the quota is even measured.
func anonymousRefusal(v InstanceSettings) error {
	switch {
	case v.RoomCreation == CreationSuperAdmin:
		return errSuperAdminOnly
	case v.StorageQuota <= 0:
		return errNoQuota
	}
	return nil
}

// quotaUsed returns the bytes the database holds under settings v's storage quota, 0 without one,
// or ErrAtQuota when it is reached.
func quotaUsed(ctx context.Context, db catalogQuerier, v InstanceSettings) (int64, error) {
	if v.StorageQuota <= 0 {
		return 0, nil
	}
	used, err := usedBytes(ctx, db)
	if err != nil {
		return 0, err
	}
	if used >= v.StorageQuota {
		return 0, fmt.Errorf("%w: the instance holds %s, at its %s storage quota", ErrAtQuota, mb(used), mb(v.StorageQuota))
	}
	return used, nil
}

// createProfileRoom is CreateProfileRoom's work in tx. The guardrails are checked in the same
// transaction as the insert, so two requests cannot both take the last room of a limit.
func createProfileRoom(ctx context.Context, tx *txn, id, name string, profileID int64, displayName, inviteHash, tokenHash, link, linkHash string, setup *RoomSetup) (*Room, error) {
	v, err := instanceSettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := creationAllowed(ctx, tx, v, profileID); err != nil {
		return nil, err
	}
	used, err := quotaUsed(ctx, tx, v)
	if err != nil {
		return nil, err
	}
	return insertProfileRoom(ctx, tx, v, used, id, name, profileID, displayName, inviteHash, tokenHash, link, linkHash, setup)
}

// insertProfileRoom creates the room once the guardrails under settings v passed, used being the
// bytes the database held then.
func insertProfileRoom(ctx context.Context, tx *txn, v InstanceSettings, used int64, id, name string, profileID int64, displayName, inviteHash, tokenHash, link, linkHash string, setup *RoomSetup) (*Room, error) {
	r, err := insertRoom(ctx, tx, id, name, profileID, setup == nil)
	if err != nil {
		return nil, err
	}
	var inv int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO invites(room_id, token_hash, created_at) VALUES(?,?,?) RETURNING id`, id, inviteHash, now()).Scan(&inv); err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}
	p, _, err := join(ctx, tx, inviteHash, displayName, tokenHash, JoinProfile{ID: profileID})
	if err != nil {
		return nil, err
	}
	// The creator's own seat: listed as their invite, revoking it lets them go like any other.
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET created_by = ? WHERE id = ?`, p.ID, inv); err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}
	if _, err := insertInvite(ctx, tx, id, linkHash, p.ID, link); err != nil {
		return nil, fmt.Errorf("create room: shareable invite: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES(?,?,?)`, id, profileID, now()); err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}
	if setup != nil {
		plugins, err := setUp(ctx, tx, r, p, setup)
		if err != nil {
			return nil, err
		}
		// A starter's documents, skills and the plugins the room kept are copied in: they count
		// too (006-AC12, 009-AC28).
		if size := setup.blobBytes() + plugins; v.StorageQuota > 0 && used+size > v.StorageQuota {
			return nil, fmt.Errorf("%w: the instance holds %s, and the starter's %s would pass its %s storage quota", ErrCreationRefused,
				mb(used), mb(size), mb(v.StorageQuota))
		}
	}
	return r, auditTx(ctx, tx, "", nil)
}

// creationAllowed returns ErrCreationRefused, with why, unless profile profileID may create one more
// room under settings v: the creation mode and its rooms limit, its own or the instance's.
func creationAllowed(ctx context.Context, tx *txn, v InstanceSettings, profileID int64) error {
	if v.RoomCreation == CreationSuperAdmin {
		return errSuperAdminOnly
	}
	var n int
	var own sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM rooms WHERE creator_profile_id = p.id), max_rooms FROM profiles p WHERE id = ?`,
		profileID).Scan(&n, &own); err != nil {
		return fmt.Errorf("create room: %w", err)
	}
	switch {
	case !own.Valid && n >= v.RoomsPerProfile:
		return fmt.Errorf("%w: you have created %d rooms, the most this instance allows per profile", ErrCreationRefused, n)
	case own.Valid && own.Int64 != UnlimitedRooms && int64(n) >= own.Int64:
		return fmt.Errorf("%w: you have created %d rooms, the most the instance admin allows your profile", ErrCreationRefused, n)
	}
	return nil
}

// SetProfileMaxRooms sets profile id's own rooms limit: n rooms, UnlimitedRooms, or with nil the
// instance's (004-AC7). The caller validated n. ErrNotFound.
func (s *Store) SetProfileMaxRooms(ctx context.Context, id int64, n *int) error {
	return s.withTx(ctx, "set profile max rooms", func(tx *txn) error {
		var v any
		if n != nil {
			v = *n
		}
		if err := execOne(ctx, tx, `UPDATE profiles SET max_rooms = ? WHERE id = ?`, v, id); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", nil)
	})
}

// RoomAdmin returns the uuid of profile profileID when it is an admin of room; ErrForbidden when
// it is not, or profileID is 0.
func (s *Store) RoomAdmin(ctx context.Context, roomID string, profileID int64) (string, error) {
	return roomAdmin(ctx, s.db, roomID, profileID)
}

func roomAdmin(ctx context.Context, db rowQuerier, roomID string, profileID int64) (string, error) {
	var uuid string
	err := db.QueryRowContext(ctx, `SELECT p.uuid FROM room_admins a JOIN profiles p ON p.id = a.profile_id WHERE a.room_id = ? AND a.profile_id = ?`,
		roomID, profileID).Scan(&uuid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrForbidden
	}
	if err != nil {
		return "", fmt.Errorf("room admin: %w", err)
	}
	return uuid, nil
}

// SetRoomAdmin appoints, or with on false removes, the profile of participant target of room as a
// room admin, and says so in the chat. by must be a room admin, checked in the same transaction,
// or nil for the super-admin, who appoints in rooms that have none; the last admin cannot step
// down. A change of nothing returns a nil message (004-AC8). ErrForbidden, ErrNotFound for a
// target not in the room, ErrNoProfile, ErrLastAdmin.
func (s *Store) SetRoomAdmin(ctx context.Context, roomID string, target int64, on bool, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "set room admin", func(tx *txn) error {
		if by != nil {
			if _, err := roomAdmin(ctx, tx, roomID, by.ProfileID); err != nil {
				return err
			}
		}
		var name string
		var profile sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT p.name, p.profile_id FROM participants p
			WHERE p.id = ? AND p.room_id = ? AND p.removed_at IS NULL`, target, roomID).Scan(&name, &profile)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("set room admin: %w", err)
		}
		if !profile.Valid {
			return ErrNoProfile
		}
		var res sql.Result
		body := fmt.Sprintf("made %s a room admin.", name)
		if on {
			res, err = tx.ExecContext(ctx, `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES(?,?,?) ON CONFLICT DO NOTHING`, roomID, profile.Int64, now())
		} else {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM room_admins WHERE room_id = ?`, roomID).Scan(&n); err != nil {
				return fmt.Errorf("set room admin: %w", err)
			}
			self := by != nil && profile.Int64 == by.ProfileID
			if n == 1 && self {
				return ErrLastAdmin
			}
			body = fmt.Sprintf("removed %s from the room admins.", name)
			if self {
				body = "stepped down as a room admin."
			}
			res, err = tx.ExecContext(ctx, `DELETE FROM room_admins WHERE room_id = ? AND profile_id = ?`, roomID, profile.Int64)
		}
		if err != nil {
			return fmt.Errorf("set room admin: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		if by == nil {
			m = &Message{RoomID: roomID, Kind: KindSystem, Sender: "admin", Body: "The admin " + body}
			err = insertMessage(ctx, tx, m)
		} else {
			m, err = memberLine(ctx, tx, roomID, by, body)
		}
		if err != nil {
			return err
		}
		return auditTx(ctx, tx, "", map[string]any{"participant": target, "name": name})
	})
	return m, err
}

// RenameParticipant gives participant target of room the name name, and says so in the chat. by
// renames themselves (005-AC1), or another participant as a room admin, checked in the same
// transaction; nil is the super-admin (005-AC21). toProfile also makes it the name of by's profile.
// Past lines keep the name they were written under. A name unchanged writes nothing: nil message.
func (s *Store) RenameParticipant(ctx context.Context, roomID string, target int64, name string, by *Participant, toProfile bool) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "rename participant", func(tx *txn) error {
		self := by != nil && by.ID == target
		if by != nil && !self {
			if _, err := roomAdmin(ctx, tx, roomID, by.ProfileID); err != nil {
				return err
			}
		}
		var old string
		err := tx.QueryRowContext(ctx, `SELECT p.name FROM participants p
			WHERE p.id = ? AND p.room_id = ? AND p.removed_at IS NULL`, target, roomID).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("rename participant: %w", err)
		}
		if self && toProfile && by.ProfileID != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE profiles SET name = ? WHERE id = ?`, name, by.ProfileID); err != nil {
				return fmt.Errorf("rename profile: %w", err)
			}
		}
		if old == name {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE participants SET name = ?, previous_name = ? WHERE id = ?`, name, old, target); err != nil {
			return fmt.Errorf("rename participant: %w", err)
		}
		switch {
		case self:
			m = &Message{RoomID: roomID, Kind: KindSystem, ParticipantID: target, Sender: name, Body: old + " is now " + name}
			err = insertMessage(ctx, tx, m)
		case by == nil:
			m = &Message{RoomID: roomID, Kind: KindSystem, Sender: "admin", Body: "The admin renamed " + old + " to " + name}
			err = insertMessage(ctx, tx, m)
		default:
			m, err = memberLine(ctx, tx, roomID, by, "renamed "+old+" to "+name)
		}
		if err != nil {
			return err
		}
		return auditTx(ctx, tx, "", map[string]any{"participant": target, "from": old, "to": name})
	})
	return m, err
}
