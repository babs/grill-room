package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrSelfMerge is returned when a profile is merged into itself (011-AC10).
var ErrSelfMerge = errors.New("a profile cannot be merged into itself")

// MergeProfile is one side of a merge. Email is for the admin only.
type MergeProfile struct {
	ID    int64  `json:"id"`
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// MergeRoom is a room either profile can enter; both set when both have a seat there.
type MergeRoom struct {
	RoomID   string `json:"room_id"`
	RoomName string `json:"room_name"`
	Keep     bool   `json:"keep"`
	Merge    bool   `json:"merge"`
}

// MergeRename is a personal skill of the merged profile renamed on a collision.
type MergeRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// MergeSkills are both profiles' personal skill names and the renames the merge makes.
type MergeSkills struct {
	Keep    []string      `json:"keep"`
	Merge   []string      `json:"merge"`
	Renamed []MergeRename `json:"renamed"`
}

// MergeCount counts one kind of item on each side.
type MergeCount struct {
	Keep  int `json:"keep"`
	Merge int `json:"merge"`
}

// MergeAdmin names the rooms each profile is a room admin of.
type MergeAdmin struct {
	Keep  []string `json:"keep"`
	Merge []string `json:"merge"`
}

// MergeLimit is a per-profile limit the kept profile passes after the merge (011-AC14).
type MergeLimit struct {
	Kind  string `json:"kind"`
	After int    `json:"after"`
	Limit int    `json:"limit"`
}

// MergePreview is what merging Merge into Keep would do, before anything changes (011-AC10).
type MergePreview struct {
	Keep  MergeProfile `json:"keep"`
	Merge MergeProfile `json:"merge"`
	Rooms []MergeRoom  `json:"rooms"`
	// Seats counts every seat, revoked ones too, as MergeMoved.Seats does.
	Seats        MergeCount   `json:"seats"`
	Skills       MergeSkills  `json:"skills"`
	SharedSkills MergeCount   `json:"shared_skills"`
	Templates    MergeCount   `json:"templates"`
	Starters     MergeCount   `json:"starters"`
	RoomsCreated MergeCount   `json:"rooms_created"`
	RoomAdmin    MergeAdmin   `json:"room_admin"`
	OverLimits   []MergeLimit `json:"over_limits"`
	EmailAfter   string       `json:"email_after"`
}

// MergeMoved counts what a merge moved, per kind.
type MergeMoved struct {
	Seats        int64 `json:"seats"`
	Skills       int64 `json:"skills"`
	SharedSkills int64 `json:"shared_skills"`
	Templates    int64 `json:"templates"`
	Starters     int64 `json:"starters"`
	RoomsCreated int64 `json:"rooms_created"`
	RoomAdmins   int64 `json:"room_admins"`
	Sessions     int64 `json:"sessions"`
}

// mergeFault, set by tests, fails a merge after its moves and before it deletes the merged profile.
var mergeFault func() error

// MergePreview returns what MergeProfiles(keep, merge) would do. ErrSelfMerge, ErrNotFound.
func (s *Store) MergePreview(ctx context.Context, keep, merge int64) (*MergePreview, error) {
	var p *MergePreview
	err := s.withTx(ctx, "merge preview", func(tx *sql.Tx) error {
		var err error
		p, err = mergePreview(ctx, tx, keep, merge)
		return err
	})
	return p, err
}

func mergePreview(ctx context.Context, db *sql.Tx, keep, merge int64) (*MergePreview, error) {
	if keep == merge {
		return nil, ErrSelfMerge
	}
	p := &MergePreview{Rooms: []MergeRoom{}, OverLimits: []MergeLimit{}, Skills: MergeSkills{Renamed: []MergeRename{}}}
	var maxRooms sql.NullInt64
	for _, side := range []struct {
		id   int64
		into *MergeProfile
		max  *sql.NullInt64
	}{{keep, &p.Keep, &maxRooms}, {merge, &p.Merge, new(sql.NullInt64)}} {
		side.into.ID = side.id
		err := db.QueryRowContext(ctx, `SELECT COALESCE(p.uuid, ''), `+displayNameSQL+`, COALESCE(p.email, ''), p.max_rooms FROM profiles p WHERE p.id = ?`,
			side.id).Scan(&side.into.UUID, &side.into.Name, &side.into.Email, side.max)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("merge preview: %w", err)
		}
	}
	p.EmailAfter = p.Keep.Email
	if p.EmailAfter == "" {
		p.EmailAfter = p.Merge.Email
	}
	err := eachRow(ctx, db, func(rows *sql.Rows) error {
		var r MergeRoom
		if err := rows.Scan(&r.RoomID, &r.RoomName, &r.Keep, &r.Merge); err != nil {
			return err
		}
		p.Rooms = append(p.Rooms, r)
		return nil
	}, `SELECT r.id, r.name, MAX(pa.profile_id = ?), MAX(pa.profile_id = ?) FROM participants pa
		JOIN invites i ON i.id = pa.invite_id JOIN rooms r ON r.id = pa.room_id
		WHERE pa.profile_id IN (?, ?) AND i.revoked_at IS NULL GROUP BY r.id ORDER BY r.name, r.id`, keep, merge, keep, merge)
	if err != nil {
		return nil, fmt.Errorf("merge preview: rooms: %w", err)
	}
	for _, l := range []struct {
		into  *[]string
		query string
		id    int64
	}{
		{&p.Skills.Keep, `SELECT name FROM skills WHERE profile_id = ? ORDER BY name`, keep},
		{&p.Skills.Merge, `SELECT name FROM skills WHERE profile_id = ? ORDER BY name`, merge},
		{&p.RoomAdmin.Keep, `SELECT r.name FROM room_admins a JOIN rooms r ON r.id = a.room_id WHERE a.profile_id = ? ORDER BY r.name`, keep},
		{&p.RoomAdmin.Merge, `SELECT r.name FROM room_admins a JOIN rooms r ON r.id = a.room_id WHERE a.profile_id = ? ORDER BY r.name`, merge},
	} {
		*l.into = []string{}
		err := eachRow(ctx, db, func(rows *sql.Rows) error {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			*l.into = append(*l.into, n)
			return nil
		}, l.query, l.id)
		if err != nil {
			return nil, fmt.Errorf("merge preview: %w", err)
		}
	}
	p.Skills.Renamed = skillRenames(p.Skills.Keep, p.Skills.Merge)
	for _, side := range []struct {
		id int64
		n  []*int
	}{
		{keep, []*int{&p.SharedSkills.Keep, &p.Templates.Keep, &p.Starters.Keep, &p.RoomsCreated.Keep, &p.Seats.Keep}},
		{merge, []*int{&p.SharedSkills.Merge, &p.Templates.Merge, &p.Starters.Merge, &p.RoomsCreated.Merge, &p.Seats.Merge}},
	} {
		err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM skills WHERE publisher_id = ?), (SELECT count(*) FROM persona_templates WHERE profile_id = ?),
			(SELECT count(*) FROM starters WHERE profile_id = ?), (SELECT count(*) FROM rooms WHERE creator_profile_id = ?),
			(SELECT count(*) FROM participants WHERE profile_id = ?)`,
			side.id, side.id, side.id, side.id, side.id).Scan(side.n[0], side.n[1], side.n[2], side.n[3], side.n[4])
		if err != nil {
			return nil, fmt.Errorf("merge preview: counts: %w", err)
		}
	}
	v, err := instanceSettings(ctx, db)
	if err != nil {
		return nil, err
	}
	roomsLimit := v.RoomsPerProfile
	if maxRooms.Valid {
		roomsLimit = int(maxRooms.Int64)
	}
	// The limits PublishSkill, PublishTemplate, SaveStarter and creationAllowed check.
	for _, l := range []MergeLimit{
		{"skills", len(p.Skills.Keep) + len(p.Skills.Merge) + p.SharedSkills.Keep + p.SharedSkills.Merge, MaxProfileSkills},
		{"templates", p.Templates.Keep + p.Templates.Merge, MaxProfileTemplates},
		{"starters", p.Starters.Keep + p.Starters.Merge, MaxProfileStarters},
		{"rooms_created", p.RoomsCreated.Keep + p.RoomsCreated.Merge, roomsLimit},
	} {
		if l.Limit != UnlimitedRooms && l.After > l.Limit {
			p.OverLimits = append(p.OverLimits, l)
		}
	}
	return p, nil
}

// skillRenames gives each merged name the kept profile already has the first free -2, -3, ...
// suffix, cutting the name so it stays a valid skill name (64 characters at most).
func skillRenames(keep, merge []string) []MergeRename {
	taken := map[string]bool{}
	for _, n := range keep {
		taken[n] = true
	}
	var clash []string
	for _, n := range merge {
		if taken[n] {
			clash = append(clash, n)
		} else {
			taken[n] = true
		}
	}
	out := []MergeRename{}
	for _, n := range clash {
		for i := 2; ; i++ {
			suffix := "-" + strconv.Itoa(i)
			base := n
			if len(base)+len(suffix) > 64 {
				base = strings.TrimRight(base[:64-len(suffix)], "-")
			}
			if to := base + suffix; !taken[to] {
				taken[to] = true
				out = append(out, MergeRename{From: n, To: to})
				break
			}
		}
	}
	return out
}

// MergeProfiles moves everything of profile merge to profile keep and deletes merge, in one
// transaction with the audit entry ctx carries (011-AC11 to 011-AC13); then run Checkpoint. keep keeps its name, rooms
// limit, nudge snooze and email, or takes merge's when it has none. ErrSelfMerge, ErrNotFound.
func (s *Store) MergeProfiles(ctx context.Context, keep, merge int64) (*MergeProfile, MergeMoved, error) {
	var kept MergeProfile
	var moved MergeMoved
	err := s.withTx(ctx, "merge profiles", func(tx *sql.Tx) error {
		p, err := mergePreview(ctx, tx, keep, merge)
		if err != nil {
			return err
		}
		kept = p.Keep
		// A without a chosen name shows its newest seat's: B's seats would rename it (011-AC12).
		if err := execOne(ctx, tx, `UPDATE profiles SET name = COALESCE(name, NULLIF(?, '')) WHERE id = ?`, p.Keep.Name, keep); err != nil {
			return err
		}
		exec := func(n *int64, query string, args ...any) error {
			res, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return fmt.Errorf("merge profiles: %w", err)
			}
			if n != nil {
				if *n, err = res.RowsAffected(); err != nil {
					return fmt.Errorf("merge profiles: %w", err)
				}
			}
			return nil
		}
		to := map[string]string{}
		for _, r := range p.Skills.Renamed {
			to[r.From] = r.To
		}
		for _, name := range p.Skills.Merge {
			if err := exec(nil, `UPDATE skills SET profile_id = ?, name = ? WHERE profile_id = ? AND name = ?`, keep, cmp.Or(to[name], name), merge, name); err != nil {
				return err
			}
		}
		moved.Skills = int64(len(p.Skills.Merge))
		// B's room admin roles become A's where A has none; the rest go with B's rows.
		steps := []struct {
			n     *int64
			query string
		}{
			{&moved.Seats, `UPDATE participants SET profile_id = ?1 WHERE profile_id = ?2`},
			{&moved.SharedSkills, `UPDATE skills SET publisher_id = ?1 WHERE publisher_id = ?2`},
			{nil, `UPDATE skill_versions SET author_id = ?1 WHERE author_id = ?2`},
			{&moved.Templates, `UPDATE persona_templates SET profile_id = ?1 WHERE profile_id = ?2`},
			{&moved.Starters, `UPDATE starters SET profile_id = ?1 WHERE profile_id = ?2`},
			{&moved.RoomsCreated, `UPDATE rooms SET creator_profile_id = ?1 WHERE creator_profile_id = ?2`},
			{nil, `INSERT INTO room_admins(room_id, profile_id, created_at) SELECT room_id, ?1, created_at FROM room_admins WHERE profile_id = ?2 ON CONFLICT DO NOTHING`},
			{&moved.RoomAdmins, `DELETE FROM room_admins WHERE profile_id = ?2`},
			{&moved.Sessions, `UPDATE profile_sessions SET profile_id = ?1 WHERE profile_id = ?2`},
			{nil, `DELETE FROM magic_links WHERE profile_id = ?2`},
			{nil, `DELETE FROM recovery_links WHERE profile_id = ?2`},
			{nil, `UPDATE profiles SET skills_disclaimer_at = COALESCE(skills_disclaimer_at, (SELECT skills_disclaimer_at FROM profiles WHERE id = ?2)) WHERE id = ?1`},
		}
		for _, st := range steps {
			if err := exec(st.n, st.query, keep, merge); err != nil {
				return err
			}
		}
		if p.Keep.Email == "" && p.Merge.Email != "" {
			// The address is UNIQUE: B lets go of it before A takes it.
			if err := exec(nil, `UPDATE profiles SET email = NULL WHERE id = ?`, merge); err != nil {
				return err
			}
			if err := exec(nil, `UPDATE profiles SET email = ? WHERE id = ?`, p.Merge.Email, keep); err != nil {
				return err
			}
		}
		if mergeFault != nil {
			if err := mergeFault(); err != nil {
				return err
			}
		}
		// Never an email: the entry outlives both profiles' addresses (011-AC13).
		if err := auditTx(ctx, tx, "", map[string]any{"keep_uuid": p.Keep.UUID, "keep_name": p.Keep.Name,
			"merge_uuid": p.Merge.UUID, "merge_name": p.Merge.Name, "moved": moved}); err != nil {
			return err
		}
		return execOne(ctx, tx, `DELETE FROM profiles WHERE id = ?`, merge)
	})
	if err != nil {
		return nil, MergeMoved{}, err
	}
	return &kept, moved, nil
}

// Checkpoint rewrites the WAL into the main file, so rows a delete, merge or purge removed do not
// linger there. Callers run it after the commit; a failure leaves the change done.
func (s *Store) Checkpoint(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

// EmptyProfileIdle is how long an empty profile stays after its last activity (011-AC15).
const EmptyProfileIdle = 24 * time.Hour

// purgeBatch bounds one purge run so its audit entry keeps every uuid under MaxAuditDetails.
// ponytail: 90 per run, hourly; the next run takes the rest.
const purgeBatch = 90

// PurgeEmptyProfiles deletes, as DeleteProfile does, the empty profiles whose last activity is
// older than idle before now, and returns their uuids. Empty: no email, no seat with a valid invite,
// no personal or published skill, template, starter or room admin role; activity: creation, joins,
// messages and sign-ins (011-AC15). A run that deletes writes the audit entry ctx carries (011-AC16);
// then run Checkpoint.
// only, when set, restricts the run to those profile ids.
func (s *Store) PurgeEmptyProfiles(ctx context.Context, now time.Time, idle time.Duration, only ...int64) ([]string, error) {
	uuids := []string{}
	err := s.withTx(ctx, "purge empty profiles", func(tx *sql.Tx) error {
		cutoff := now.Add(-idle)
		var ids []int64
		err := eachRow(ctx, tx, func(rows *sql.Rows) error {
			var id int64
			var uuid string
			var ts [4]string
			if err := rows.Scan(&id, &uuid, &ts[0], &ts[1], &ts[2], &ts[3]); err != nil {
				return err
			}
			if len(only) > 0 && !slices.Contains(only, id) {
				return nil
			}
			for _, v := range ts {
				if v != "" && !parseTS(v).Before(cutoff) {
					return nil
				}
			}
			if len(ids) < purgeBatch {
				ids, uuids = append(ids, id), append(uuids, uuid)
			}
			return nil
		}, `SELECT p.id, COALESCE(p.uuid, ''), p.created_at,
			COALESCE((SELECT MAX(joined_at) FROM participants WHERE profile_id = p.id), ''),
			COALESCE((SELECT m.ts FROM messages m WHERE m.id = (SELECT MAX(m2.id) FROM participants pa
				JOIN messages m2 ON m2.participant_id = pa.id WHERE pa.profile_id = p.id)), ''),
			COALESCE((SELECT MAX(created_at) FROM profile_sessions WHERE profile_id = p.id), '')
			FROM profiles p WHERE p.email IS NULL
			AND NOT EXISTS (SELECT 1 FROM participants pa JOIN invites i ON i.id = pa.invite_id WHERE pa.profile_id = p.id AND i.revoked_at IS NULL)
			AND NOT EXISTS (SELECT 1 FROM skills WHERE profile_id = p.id)
			AND NOT EXISTS (SELECT 1 FROM skills WHERE publisher_id = p.id)
			AND NOT EXISTS (SELECT 1 FROM persona_templates WHERE profile_id = p.id)
			AND NOT EXISTS (SELECT 1 FROM starters WHERE profile_id = p.id)
			AND NOT EXISTS (SELECT 1 FROM room_admins WHERE profile_id = p.id)
			ORDER BY p.id`)
		if err != nil {
			return fmt.Errorf("purge empty profiles: %w", err)
		}
		for _, id := range ids {
			if err := execOne(ctx, tx, `DELETE FROM profiles WHERE id = ?`, id); err != nil {
				return err
			}
		}
		if len(ids) == 0 {
			return nil
		}
		return auditTx(ctx, tx, "", map[string]any{"count": len(ids), "uuids": uuids})
	})
	if err != nil {
		return nil, err
	}
	return uuids, nil
}
