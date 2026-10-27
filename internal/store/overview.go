package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// RoomVolume is what a room stores, in bytes, by kind (004-AC2). Lengths come from SQL aggregates:
// no blob is read.
type RoomVolume struct {
	Uploads       int64 `json:"uploads"`
	Revisions     int64 `json:"revisions"`
	Attachments   int64 `json:"attachments"`
	AgentSessions int64 `json:"agent_sessions"`
	// Transcripts are the room's lines, their rendered HTML and their earlier versions.
	Transcripts int64 `json:"transcripts"`
	// RoomSkills are the versions of the room's own skills (004-AC10).
	RoomSkills int64 `json:"room_skills"`
	// Plugins are the snapshots of the room's plugins (009-AC9).
	Plugins int64 `json:"plugins"`
	Total   int64 `json:"total"`
}

// RoomOverview is one row of the admin's rooms view (004-AC2).
type RoomOverview struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	LastActivity time.Time `json:"last_activity"`
	// Participants counts the valid seats.
	Participants      int   `json:"participants"`
	Personas          int   `json:"personas"`
	HostParticipantID int64 `json:"host_participant_id,omitempty"`
	// QuotaBytes is the room's own quota, 0 when it follows the instance default; Quota is the one
	// in force, its own or the default.
	QuotaBytes int64      `json:"quota_bytes"`
	Quota      int64      `json:"quota"`
	Volume     RoomVolume `json:"volume"`
	// CreatedFrom is the starter version the room was created from (006-AC11).
	CreatedFrom *Provenance `json:"created_from,omitempty"`
	// PrivatePlugins names the plugins from private repos: the export dialog warns of them (009-AC26).
	PrivatePlugins []string `json:"private_plugins,omitempty"`
}

// Counted is what the room's quota counts: upload originals, annotation attachments and plugin
// snapshots (009-AC9).
func (v RoomVolume) Counted() int64 { return v.Uploads + v.Attachments + v.Plugins }

// RoomOverviews lists every room with its counts, volume and quota, defaultQuota for those without
// their own, newest first.
// ponytail: sums every message row on each call, O(messages), without the write lock; keep
// running totals per room if instances grow past that.
func (s *Store) RoomOverviews(ctx context.Context, defaultQuota int64) ([]RoomOverview, error) {
	return s.roomOverviews(ctx, defaultQuota, "")
}

// RoomOverview is RoomOverviews for room alone. ErrNotFound.
func (s *Store) RoomOverview(ctx context.Context, roomID string, defaultQuota int64) (*RoomOverview, error) {
	list, err := s.roomOverviews(ctx, defaultQuota, roomID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// roomActivitySQL is room r's last message time. A voice paragraph dates it by its last append,
// and grows past typed messages, so its latest non-typed message counts too (018-AC5, 020-AC17).
const roomActivitySQL = `GREATEST((SELECT COALESCE(appended_at, ts) FROM messages WHERE room_id = r.id ORDER BY id DESC LIMIT 1),
	(SELECT COALESCE(appended_at, ts) FROM messages WHERE room_id = r.id AND ` + nonText + ` ORDER BY id DESC LIMIT 1))`

// profileActivitySQL is profile p's last line time, as roomActivitySQL across its seats.
const profileActivitySQL = `GREATEST((SELECT COALESCE(m.appended_at, m.ts) FROM messages m WHERE m.id = (SELECT MAX(m2.id) FROM participants pa
	JOIN messages m2 ON m2.participant_id = pa.id WHERE pa.profile_id = p.id)),
	(SELECT COALESCE(m.appended_at, m.ts) FROM messages m WHERE m.id = (SELECT MAX(m2.id) FROM participants pa
	JOIN messages m2 ON m2.participant_id = pa.id WHERE pa.profile_id = p.id AND m2.` + nonText + `)))`

// roomOverviews lists the rooms, or only room roomID when set.
func (s *Store) roomOverviews(ctx context.Context, defaultQuota int64, roomID string) ([]RoomOverview, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.name, r.created_at, COALESCE(r.host_participant_id, 0), COALESCE(r.quota_bytes, 0), COALESCE(r.quota_bytes, ?),
		COALESCE(`+roomActivitySQL+`, ''),
		(SELECT COUNT(*) FROM participants p WHERE p.room_id = r.id AND p.removed_at IS NULL),
		(SELECT COUNT(*) FROM room_personas WHERE room_id = r.id),
		(SELECT COALESCE(SUM(size), 0) FROM uploads WHERE room_id = r.id),
		(SELECT COALESCE(SUM(octet_length(v.body)), 0) FROM revisions v JOIN documents d ON d.id = v.document_id WHERE d.room_id = r.id),
		(SELECT COALESCE(SUM(t.size), 0) FROM attachments t JOIN annotation_messages m ON m.id = t.message_id
			JOIN annotations a ON a.id = m.annotation_id WHERE a.room_id = r.id),
		(SELECT COALESCE(SUM(length(data)), 0) FROM agent_sessions WHERE room_id = r.id),
		(SELECT COALESCE(SUM(octet_length(body) + COALESCE(octet_length(html), 0)), 0) FROM messages WHERE room_id = r.id)
		+ (SELECT COALESCE(SUM(octet_length(v.body)), 0) FROM message_versions v JOIN messages m ON m.id = v.message_id WHERE m.room_id = r.id),
		(SELECT COALESCE(SUM(length(v.data)), 0) FROM skill_versions v JOIN skills k ON k.id = v.skill_id WHERE k.room_id = r.id),
		(SELECT COALESCE(SUM(length(sn.data)), 0) FROM plugin_snapshots sn JOIN plugin_versions v ON v.id = sn.version_id
			JOIN room_plugins p ON p.id = v.plugin_id WHERE p.room_id = r.id),
		r.created_from,
		(SELECT COALESCE(jsonb_agg(name ORDER BY name), '[]')::text FROM room_plugins WHERE room_id = r.id AND private = 1 AND removed_at IS NULL)
		FROM rooms r WHERE ? = '' OR r.id = ? ORDER BY r.created_at DESC, r.id DESC`, defaultQuota, roomID, roomID)
	if err != nil {
		return nil, fmt.Errorf("room overviews: %w", err)
	}
	defer rows.Close()
	out := []RoomOverview{}
	for rows.Next() {
		var o RoomOverview
		var created, last string
		var from sql.NullString
		var private string
		v := &o.Volume
		if err := rows.Scan(&o.ID, &o.Name, &created, &o.HostParticipantID, &o.QuotaBytes, &o.Quota, &last, &o.Participants, &o.Personas,
			&v.Uploads, &v.Revisions, &v.Attachments, &v.AgentSessions, &v.Transcripts, &v.RoomSkills, &v.Plugins, &from, &private); err != nil {
			return nil, fmt.Errorf("scan room overview: %w", err)
		}
		if err := json.Unmarshal([]byte(private), &o.PrivatePlugins); err != nil {
			return nil, fmt.Errorf("scan room overview: %w", err)
		}
		v.Total = v.Uploads + v.Revisions + v.Attachments + v.AgentSessions + v.Transcripts + v.RoomSkills + v.Plugins
		o.CreatedAt, o.CreatedFrom = parseTS(created), parseProvenance(from)
		o.LastActivity = o.CreatedAt
		if last != "" {
			o.LastActivity = parseTS(last)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetRoomQuota sets the room's own quota in bytes, or with 0 returns it to the instance default
// (004-AC9). ErrNotFound if the room does not exist.
func (s *Store) SetRoomQuota(ctx context.Context, roomID string, quota int64) error {
	return s.withTx(ctx, "set room quota", func(tx *txn) error {
		if err := execOne(ctx, tx, `UPDATE rooms SET quota_bytes = ? WHERE id = ?`, nullID(quota), roomID); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", nil)
	})
}

// AdminProfile is one row of the admin's profiles view (004-AC3): the only place an email shows,
// apart from its own profile.
type AdminProfile struct {
	Profile
	UUID string `json:"uuid"`
	// Name is the one the profile chose (005-AC1), else the name it last joined a room with.
	Name string `json:"name"`
	// Skills counts the personal skills it owns and the shared ones it published.
	Skills       int       `json:"skills"`
	LastActivity time.Time `json:"last_activity"`
	// MaxRooms is its own limit on the rooms it creates: nil follows the instance's, UnlimitedRooms
	// lifts it (004-AC7). RoomsCreated counts those it has.
	MaxRooms     *int `json:"max_rooms"`
	RoomsCreated int  `json:"rooms_created"`
}

// Profiles lists every profile with its email, rooms, skill count and last activity: its last
// line, join or creation, whichever is latest.
func (s *Store) Profiles(ctx context.Context) ([]AdminProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.email, p.created_at, COALESCE(p.uuid, ''),
		`+displayNameSQL+`,
		COALESCE((SELECT joined_at FROM participants WHERE profile_id = p.id ORDER BY id DESC LIMIT 1), ''),
		(SELECT COUNT(*) FROM skills WHERE profile_id = p.id OR publisher_id = p.id), p.max_rooms,
		(SELECT COUNT(*) FROM rooms WHERE creator_profile_id = p.id),
		COALESCE(`+profileActivitySQL+`, '')
		FROM profiles p ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("query profiles: %w", err)
	}
	var out []AdminProfile
	for rows.Next() {
		var p AdminProfile
		var email sql.NullString
		var created, joined, line string
		var maxRooms sql.NullInt64
		if err := rows.Scan(&p.ID, &email, &created, &p.UUID, &p.Name, &joined, &p.Skills, &maxRooms, &p.RoomsCreated, &line); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		p.Email, p.CreatedAt, p.LastActivity = email.String, parseTS(created), parseTS(created)
		if maxRooms.Valid {
			n := int(maxRooms.Int64)
			p.MaxRooms = &n
		}
		for _, ts := range []string{joined, line} {
			if t := parseTS(ts); t.After(p.LastActivity) {
				p.LastActivity = t
			}
		}
		out = append(out, p)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query profiles: %w", err)
	}
	// ponytail: one rooms query per profile; a single join if the admin list grows past a few thousand.
	for i := range out {
		if out[i].Rooms, err = s.profileRooms(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []AdminProfile{}
	}
	return out, nil
}
