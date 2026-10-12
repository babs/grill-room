package store

import (
	"context"
	"database/sql"
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
	// RoomSkills stays 0 until rooms own skills (004-AC10).
	RoomSkills int64 `json:"room_skills"`
	Total      int64 `json:"total"`
}

// RoomOverview is one row of the admin's rooms view (004-AC2).
type RoomOverview struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	LastActivity time.Time `json:"last_activity"`
	// Participants counts those whose invite still works.
	Participants      int   `json:"participants"`
	Personas          int   `json:"personas"`
	HostParticipantID int64 `json:"host_participant_id,omitempty"`
	// QuotaBytes is the room's own quota, 0 when it follows the instance default; Quota is the one
	// in force, its own or the default.
	QuotaBytes int64      `json:"quota_bytes"`
	Quota      int64      `json:"quota"`
	Volume     RoomVolume `json:"volume"`
}

// Counted is what the room's quota counts: upload originals and annotation attachments.
func (v RoomVolume) Counted() int64 { return v.Uploads + v.Attachments }

// RoomOverviews lists every room with its counts, volume and quota, defaultQuota for those without
// their own, newest first.
// ponytail: sums every message row on each call (~270 ms at 200k lines, holding the one connection);
// keep running totals per room if instances grow past that.
func (s *Store) RoomOverviews(ctx context.Context, defaultQuota int64) ([]RoomOverview, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.name, r.created_at, COALESCE(r.host_participant_id, 0), COALESCE(r.quota_bytes, 0), COALESCE(r.quota_bytes, ?),
		COALESCE((SELECT ts FROM messages WHERE room_id = r.id ORDER BY id DESC LIMIT 1), ''),
		(SELECT COUNT(*) FROM participants p JOIN invites i ON i.id = p.invite_id WHERE p.room_id = r.id AND i.revoked_at IS NULL),
		(SELECT COUNT(*) FROM room_personas WHERE room_id = r.id),
		(SELECT COALESCE(SUM(size), 0) FROM uploads WHERE room_id = r.id),
		(SELECT COALESCE(SUM(octet_length(v.body)), 0) FROM revisions v JOIN documents d ON d.id = v.document_id WHERE d.room_id = r.id),
		(SELECT COALESCE(SUM(t.size), 0) FROM attachments t JOIN annotation_messages m ON m.id = t.message_id
			JOIN annotations a ON a.id = m.annotation_id WHERE a.room_id = r.id),
		(SELECT COALESCE(SUM(length(data)), 0) FROM agent_sessions WHERE room_id = r.id),
		(SELECT COALESCE(SUM(octet_length(body) + COALESCE(octet_length(html), 0)), 0) FROM messages WHERE room_id = r.id)
		+ (SELECT COALESCE(SUM(octet_length(v.body)), 0) FROM message_versions v JOIN messages m ON m.id = v.message_id WHERE m.room_id = r.id)
		FROM rooms r ORDER BY r.rowid DESC`, defaultQuota)
	if err != nil {
		return nil, fmt.Errorf("room overviews: %w", err)
	}
	defer rows.Close()
	out := []RoomOverview{}
	for rows.Next() {
		var o RoomOverview
		var created, last string
		v := &o.Volume
		if err := rows.Scan(&o.ID, &o.Name, &created, &o.HostParticipantID, &o.QuotaBytes, &o.Quota, &last, &o.Participants, &o.Personas,
			&v.Uploads, &v.Revisions, &v.Attachments, &v.AgentSessions, &v.Transcripts); err != nil {
			return nil, fmt.Errorf("scan room overview: %w", err)
		}
		v.Total = v.Uploads + v.Revisions + v.Attachments + v.AgentSessions + v.Transcripts + v.RoomSkills
		o.CreatedAt = parseTS(created)
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
	res, err := s.db.ExecContext(ctx, `UPDATE rooms SET quota_bytes = ? WHERE id = ?`, nullID(quota), roomID)
	if err != nil {
		return fmt.Errorf("set room quota %s: %w", roomID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set room quota %s: %w", roomID, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdminProfile is one row of the admin's profiles view (004-AC3): the only place an email shows,
// apart from its own profile.
type AdminProfile struct {
	Profile
	UUID string `json:"uuid"`
	// Name is the name the profile last joined a room with; profiles have none of their own.
	Name string `json:"name"`
	// Skills counts the personal skills it owns and the shared ones it published.
	Skills       int       `json:"skills"`
	LastActivity time.Time `json:"last_activity"`
}

// Profiles lists every profile with its email, rooms, skill count and last activity: its last
// line, join or creation, whichever is latest.
func (s *Store) Profiles(ctx context.Context) ([]AdminProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.email, p.created_at, COALESCE(p.uuid, ''),
		COALESCE((SELECT name FROM participants WHERE profile_id = p.id ORDER BY id DESC LIMIT 1), ''),
		COALESCE((SELECT joined_at FROM participants WHERE profile_id = p.id ORDER BY id DESC LIMIT 1), ''),
		(SELECT COUNT(*) FROM skills WHERE profile_id = p.id OR publisher_id = p.id),
		COALESCE((SELECT m.ts FROM messages m WHERE m.id = (SELECT MAX(m2.id) FROM participants pa
			JOIN messages m2 ON m2.participant_id = pa.id WHERE pa.profile_id = p.id)), '')
		FROM profiles p ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("query profiles: %w", err)
	}
	var out []AdminProfile
	for rows.Next() {
		var p AdminProfile
		var email sql.NullString
		var created, joined, line string
		if err := rows.Scan(&p.ID, &email, &created, &p.UUID, &p.Name, &joined, &p.Skills, &line); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		p.Email, p.CreatedAt, p.LastActivity = email.String, parseTS(created), parseTS(created)
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
