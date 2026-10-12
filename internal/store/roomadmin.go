package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
}

const (
	settingRoomsPerProfile = "rooms_per_profile"
	settingStorageQuota    = "storage_quota_bytes"
	settingRoomCreation    = "room_creation"
)

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func instanceSettings(ctx context.Context, db rowQuerier) (InstanceSettings, error) {
	v := InstanceSettings{RoomsPerProfile: DefaultRoomsPerProfile, RoomCreation: CreationAnyone}
	var per, quota, mode sql.NullString
	err := db.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key = ?), (SELECT value FROM settings WHERE key = ?),
		(SELECT value FROM settings WHERE key = ?)`, settingRoomsPerProfile, settingStorageQuota, settingRoomCreation).Scan(&per, &quota, &mode)
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
	return v, nil
}

// InstanceSettings returns the room creation guardrails, defaults for those never set.
func (s *Store) InstanceSettings(ctx context.Context) (InstanceSettings, error) {
	return instanceSettings(ctx, s.db)
}

// SetInstanceSettings stores v, which the caller validated.
func (s *Store) SetInstanceSettings(ctx context.Context, v InstanceSettings) error {
	return s.withTx(ctx, "set instance settings", func(tx *sql.Tx) error {
		for k, val := range map[string]string{settingRoomsPerProfile: strconv.Itoa(v.RoomsPerProfile),
			settingStorageQuota: strconv.FormatInt(v.StorageQuota, 10), settingRoomCreation: v.RoomCreation} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, val); err != nil {
				return fmt.Errorf("set instance settings: %w", err)
			}
		}
		return auditTx(ctx, tx, "", nil)
	})
}

// UsedBytes is what the database holds: its pages in use, free pages left by deletes aside.
func (s *Store) UsedBytes(ctx context.Context) (int64, error) { return usedBytes(ctx, s.db) }

func usedBytes(ctx context.Context, db rowQuerier) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `SELECT (c.page_count - f.freelist_count) * z.page_size FROM pragma_page_count() c, pragma_freelist_count() f, pragma_page_size() z`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("used bytes: %w", err)
	}
	return n, nil
}

// mb renders bytes for a refusal.
func mb(n int64) string { return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB" }

// CreateProfileRoom creates room id named name for profile profileID, which joins it as
// displayName and becomes its first room admin (004-AC7, 004-AC8). The profile joins through an
// invite of its own, inviteHash, whose token nobody holds. The guardrails are checked in the same
// transaction as the insert, so two requests cannot both take the last room of a limit.
// ErrCreationRefused, wrapped with the reason.
func (s *Store) CreateProfileRoom(ctx context.Context, id, name string, profileID int64, displayName, inviteHash, tokenHash string) (*Room, error) {
	var r *Room
	err := s.withTx(ctx, "create room", func(tx *sql.Tx) error {
		v, err := instanceSettings(ctx, tx)
		if err != nil {
			return err
		}
		if v.RoomCreation == CreationSuperAdmin {
			return fmt.Errorf("%w: only the super-admin creates rooms on this instance", ErrCreationRefused)
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
		if v.StorageQuota > 0 {
			used, err := usedBytes(ctx, tx)
			if err != nil {
				return err
			}
			if used >= v.StorageQuota {
				return fmt.Errorf("%w: the instance holds %s, at its %s storage quota", ErrCreationRefused, mb(used), mb(v.StorageQuota))
			}
		}
		if r, err = insertRoom(ctx, tx, id, name, profileID); err != nil {
			return err
		}
		var inv int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO invites(room_id, token_hash, created_at) VALUES(?,?,?) RETURNING id`, id, inviteHash, now()).Scan(&inv); err != nil {
			return fmt.Errorf("create room: %w", err)
		}
		p, _, err := join(ctx, tx, inviteHash, displayName, tokenHash, JoinProfile{ID: profileID})
		if err != nil {
			return err
		}
		// The creator's own seat: listed as their invite, revoking it lets them go like any other.
		if _, err := tx.ExecContext(ctx, `UPDATE invites SET created_by = ? WHERE id = ?`, p.ID, inv); err != nil {
			return fmt.Errorf("create room: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES(?,?,?)`, id, profileID, now()); err != nil {
			return fmt.Errorf("create room: %w", err)
		}
		return auditTx(ctx, tx, "", nil)
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// SetProfileMaxRooms sets profile id's own rooms limit: n rooms, UnlimitedRooms, or with nil the
// instance's (004-AC7). The caller validated n. ErrNotFound.
func (s *Store) SetProfileMaxRooms(ctx context.Context, id int64, n *int) error {
	return s.withTx(ctx, "set profile max rooms", func(tx *sql.Tx) error {
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
// room admin, and says so in the chat. by must be a room admin, checked in the same transaction;
// the last admin cannot step down. A change of nothing returns a nil message (004-AC8).
// ErrForbidden, ErrNotFound for a target not in the room, ErrNoProfile, ErrLastAdmin.
func (s *Store) SetRoomAdmin(ctx context.Context, roomID string, target int64, on bool, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "set room admin", func(tx *sql.Tx) error {
		if _, err := roomAdmin(ctx, tx, roomID, by.ProfileID); err != nil {
			return err
		}
		var name string
		var profile sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT p.name, p.profile_id FROM participants p JOIN invites i ON i.id = p.invite_id
			WHERE p.id = ? AND p.room_id = ? AND i.revoked_at IS NULL`, target, roomID).Scan(&name, &profile)
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
			if n == 1 && profile.Int64 == by.ProfileID {
				return ErrLastAdmin
			}
			body = fmt.Sprintf("removed %s from the room admins.", name)
			if profile.Int64 == by.ProfileID {
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
		if m, err = memberLine(ctx, tx, roomID, by, body); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", map[string]any{"participant": target, "name": name})
	})
	return m, err
}
