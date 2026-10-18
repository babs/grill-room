package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Starter libraries (006-AC1).
const (
	LibraryBuiltin  = "builtin"
	LibraryPersonal = "personal"
	LibraryInstance = "instance"
)

// Limits on what one profile keeps in the starter library, as for persona templates (006-AC4).
const (
	MaxProfileStarters = 50
	MaxStarterVersions = 100
)

// ErrBuiltin is returned when a built-in starter is changed, published or deleted.
var ErrBuiltin = errors.New("built-in starters are read-only: duplicate one to change it")

// StarterField is a value asked when a room is created from the starter: {{Key}} in the agenda
// and the default room name becomes it (006-AC10, 006-AC16).
type StarterField struct {
	Label    string `json:"label"`
	Key      string `json:"key"`
	Required bool   `json:"required,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

// StarterPersona is a full copy of a persona, with at most one automatic trigger (006-AC18) and
// its active skills, by name among the starter's skills.
type StarterPersona struct {
	PersonaFields
	EveryN       int      `json:"every_n,omitempty"`
	EveryVoice   int      `json:"every_voice,omitempty"`
	EveryMinutes int      `json:"every_minutes,omitempty"`
	SelfCall     bool     `json:"self_call,omitempty"`
	WatchesNotes bool     `json:"watches_notes,omitempty"`
	Autocompact  int      `json:"autocompact,omitempty"`
	Skills       []string `json:"skills,omitempty"`
	// Plugins are the plugins it has on, by name among the starter's plugins (009-AC28).
	Plugins []string `json:"plugins,omitempty"`
	// Speaks, Voice and VoiceRate are absent from a starter saved before 013: it reads as speaking,
	// with the default voice and rate 1 (013-AC30).
	Speaks    *bool   `json:"speaks,omitempty"`
	Voice     string  `json:"voice,omitempty"`
	VoiceRate float64 `json:"voice_rate,omitempty"`
}

// SpeaksInCall is p.Speaks with a starter from before 013 read as on.
func (p StarterPersona) SpeaksInCall() bool { return p.Speaks == nil || *p.Speaks }

// StarterSkill is a skill frozen at one version; File names its tar among the version's files.
type StarterSkill struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Author      string `json:"author"`
	Description string `json:"description"`
	Files       int    `json:"files"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	File        string `json:"file"`
}

// StarterPlugin is a room plugin at its running version (009-AC28); File names its snapshot tar
// among the version's files. Private ones came through an AI host, and update through one.
type StarterPlugin struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Ref     string `json:"ref"`
	RefName string `json:"ref_name"`
	Entry   string `json:"entry,omitempty"`
	Private bool   `json:"private,omitempty"`
	Commit  string `json:"commit"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Files   int    `json:"files"`
	File    string `json:"file"`
	// Imported tells that its files came from an archive, not from the repo (009-AC27).
	Imported bool `json:"imported,omitempty"`
}

// StarterDocument is a document at one revision: Text is what the room shows (an agent document's
// body, an upload's text or Markdown copy); File names an upload's original among the version's
// files.
type StarterDocument struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Format      string `json:"format"`
	ContentType string `json:"content_type,omitempty"`
	Text        string `json:"text,omitempty"`
	File        string `json:"file,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

// StarterChain is the room's auto-turn settings.
type StarterChain struct {
	Budget    int  `json:"budget"`
	Suspended bool `json:"suspended,omitempty"`
	Parallel  bool `json:"parallel_turns,omitempty"`
}

// StarterManifest is everything a starter version holds but its blobs (006-AC6).
type StarterManifest struct {
	Title       string           `json:"title"`
	Description string           `json:"description"`
	RoomName    string           `json:"room_name"`
	Agenda      string           `json:"agenda"`
	Fields      []StarterField   `json:"fields"`
	Personas    []StarterPersona `json:"personas"`
	Skills      []StarterSkill   `json:"skills"`
	// Plugins is absent from a starter saved before 009.
	Plugins   []StarterPlugin   `json:"plugins,omitempty"`
	Documents []StarterDocument `json:"documents"`
	Chain     StarterChain      `json:"chain"`
	Glossary  string            `json:"glossary,omitempty"`
	// Speech is absent from a starter saved before 013: the defaults (013-AC30).
	Speech *Speech `json:"speech,omitempty"`
}

// Starter is a starter of the library as its latest version has it.
type Starter struct {
	ID        int64  `json:"id"`
	Builtin   string `json:"builtin,omitempty"`
	Library   string `json:"library"`
	ProfileID int64  `json:"-"`
	// Mine tells the asking profile that it owns the starter: it may edit, publish and delete it.
	Mine        bool   `json:"mine"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Author      string `json:"author"`
	Version     int    `json:"version"`
	Versions    int    `json:"versions"`
	// Size is what all its versions hold, against the instance storage quota (006-AC4).
	Size      int64     `json:"size"`
	UpdatedAt time.Time `json:"updated_at"`
}

const starterCols = `SELECT s.id, COALESCE(s.builtin, ''), s.library, COALESCE(s.profile_id, 0), v.n, v.author,
	COALESCE(v.manifest::jsonb->>'title', ''), COALESCE(v.manifest::jsonb->>'description', ''), v.created_at,
	(SELECT count(*) FROM starter_versions WHERE starter_id = s.id), (SELECT SUM(size) FROM starter_versions WHERE starter_id = s.id)
	FROM starters s JOIN starter_versions v ON v.starter_id = s.id AND v.n = (SELECT MAX(n) FROM starter_versions WHERE starter_id = s.id) `

// visibleTo is the starters profile viewer sees: the built-ins, the instance's and its own.
const visibleTo = `(s.library != 'personal' OR s.profile_id = ?)`

func scanStarter(sc scanner, viewer int64) (*Starter, error) {
	var st Starter
	var updated string
	err := sc.Scan(&st.ID, &st.Builtin, &st.Library, &st.ProfileID, &st.Version, &st.Author, &st.Title, &st.Description, &updated, &st.Versions, &st.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan starter: %w", err)
	}
	st.UpdatedAt, st.Mine = parseTS(updated), viewer != 0 && st.ProfileID == viewer
	return &st, nil
}

// Starters lists the starters profile viewer sees, built-ins first, then its own, then the
// instance's; viewer 0 sees the built-ins and the instance's.
func (s *Store) Starters(ctx context.Context, viewer int64) ([]Starter, error) {
	rows, err := s.db.QueryContext(ctx, starterCols+`WHERE `+visibleTo+`
		ORDER BY CASE s.library WHEN 'builtin' THEN 0 WHEN 'personal' THEN 1 ELSE 2 END, s.id`, viewer)
	if err != nil {
		return nil, fmt.Errorf("starters: %w", err)
	}
	defer rows.Close()
	out := []Starter{}
	for rows.Next() {
		st, err := scanStarter(rows, viewer)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

// StarterVersion returns starter id as viewer sees it, with the manifest of its version n, the
// latest for 0: the author, title and date are that version's. ErrNotFound for a starter viewer
// does not see or a version it lacks.
func (s *Store) StarterVersion(ctx context.Context, id int64, n int, viewer int64) (*Starter, *StarterManifest, error) {
	return starterVersion(ctx, s.db, id, n, viewer)
}

func starterVersion(ctx context.Context, db rowQuerier, id int64, n int, viewer int64) (*Starter, *StarterManifest, error) {
	st, err := scanStarter(db.QueryRowContext(ctx, starterCols+`WHERE s.id = ? AND `+visibleTo, id, viewer), viewer)
	if err != nil {
		return nil, nil, err
	}
	if n == 0 {
		n = st.Version
	}
	var raw, created string
	err = db.QueryRowContext(ctx, `SELECT manifest, author, created_at FROM starter_versions WHERE starter_id = ? AND n = ?`, id, n).Scan(&raw, &st.Author, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("starter %d v%d: %w", id, n, err)
	}
	var m StarterManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, nil, fmt.Errorf("starter %d v%d: %w", id, n, err)
	}
	st.Version, st.Title, st.Description, st.UpdatedAt = n, m.Title, m.Description, parseTS(created)
	return st, &m, nil
}

// StarterFiles returns the blobs of starter id's version n, by name.
func (s *Store) StarterFiles(ctx context.Context, id int64, n int) (map[string][]byte, error) {
	return starterFiles(ctx, s.db, id, n)
}

func starterFiles(ctx context.Context, db rowsQuerier, id int64, n int) (map[string][]byte, error) {
	rows, err := db.QueryContext(ctx, `SELECT f.name, f.data FROM starter_files f JOIN starter_versions v ON v.id = f.version_id
		WHERE v.starter_id = ? AND v.n = ?`, id, n)
	if err != nil {
		return nil, fmt.Errorf("starter files: %w", err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var name string
		var data []byte
		if err := rows.Scan(&name, &data); err != nil {
			return nil, fmt.Errorf("starter files: %w", err)
		}
		out[name] = data
	}
	return out, rows.Err()
}

// profileNameSQL is a profile's display name: the one it chose (005-AC1), else the name it last
// joined a room with. Its two ? are the profile id.
const profileNameSQL = `COALESCE((SELECT name FROM profiles WHERE id = ?), (SELECT name FROM participants WHERE profile_id = ? ORDER BY id DESC LIMIT 1), 'someone')`

// StarterSize is what a version weighs: its manifest and its blobs.
func StarterSize(manifest []byte, files map[string][]byte) int64 {
	n := int64(len(manifest))
	for _, f := range files {
		n += int64(len(f))
	}
	return n
}

// insertStarterVersion writes version n of starter id, signed author, with its blobs.
func insertStarterVersion(ctx context.Context, tx querier, id int64, n int, author string, raw []byte, files map[string][]byte) error {
	// The list reads the manifest as jsonb, which refuses \u0000: one stored would fail it for good.
	if jsonNUL(raw) {
		return fmt.Errorf("%w: the starter's text holds a NUL character", ErrLimit)
	}
	var vid int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO starter_versions(starter_id, n, author, manifest, size, created_at) VALUES(?,?,?,?,?,?) RETURNING id`,
		id, n, author, string(raw), StarterSize(raw, files), now()).Scan(&vid); err != nil {
		return fmt.Errorf("starter version: %w", err)
	}
	for name, data := range files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO starter_files(version_id, name, data) VALUES(?,?,?)`, vid, name, data); err != nil {
			return fmt.Errorf("starter file %s: %w", name, err)
		}
	}
	return nil
}

// SaveStarter stores m with its blobs as a new personal starter of profileID, or with id as the
// next version of starter id, which only its owner may add (006-AC3). The profile's name signs it.
// A save past the instance storage quota, or past the profile's limits, is refused with ErrLimit
// and the reason (006-AC4). ErrForbidden, ErrBuiltin, ErrNotFound.
func (s *Store) SaveStarter(ctx context.Context, profileID, id int64, m *StarterManifest, files map[string][]byte) (*Starter, error) {
	err := s.withTx(ctx, "save starter", func(tx *txn) error {
		var err error
		id, err = saveStarter(ctx, tx, profileID, id, m, files)
		return err
	})
	if err != nil {
		return nil, err
	}
	st, _, err := s.StarterVersion(ctx, id, 0, profileID)
	return st, err
}

// saveStarter is SaveStarter in tx; it returns the starter's id.
func saveStarter(ctx context.Context, tx *txn, profileID, id int64, m *StarterManifest, files map[string][]byte) (int64, error) {
	if profileID == 0 {
		return 0, ErrForbidden
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return 0, fmt.Errorf("save starter: %w", err)
	}
	// Before the quota's database-wide scan, which runs under the write lock.
	if jsonNUL(raw) {
		return 0, fmt.Errorf("%w: the starter's text holds a NUL character", ErrLimit)
	}
	n := 1
	if id == 0 {
		if err := overLimit(ctx, tx, MaxProfileStarters, "starters per profile", `SELECT count(*) FROM starters WHERE profile_id = ?`, profileID); err != nil {
			return 0, err
		}
	} else {
		var owner int64
		var builtin bool
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(profile_id, 0), builtin IS NOT NULL, COALESCE((SELECT MAX(n) FROM starter_versions WHERE starter_id = s.id), 0)
			FROM starters s WHERE id = ?`, id).Scan(&owner, &builtin, &n)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return 0, ErrNotFound
		case err != nil:
			return 0, fmt.Errorf("save starter: %w", err)
		case builtin:
			return 0, ErrBuiltin
		case owner != profileID:
			return 0, ErrForbidden
		case n >= MaxStarterVersions:
			return 0, fmt.Errorf("%w: at most %d versions per starter", ErrLimit, MaxStarterVersions)
		}
		n++
	}
	// Last: it sums the whole database.
	if err := withinQuota(ctx, tx, StarterSize(raw, files)); err != nil {
		return 0, err
	}
	if id == 0 {
		if err := tx.QueryRowContext(ctx, `INSERT INTO starters(profile_id, library, created_at) VALUES(?, 'personal', ?) RETURNING id`,
			profileID, now()).Scan(&id); err != nil {
			return 0, fmt.Errorf("save starter: %w", err)
		}
	}
	var author string
	if err := tx.QueryRowContext(ctx, `SELECT `+profileNameSQL, profileID, profileID).Scan(&author); err != nil {
		return 0, fmt.Errorf("save starter: %w", err)
	}
	return id, insertStarterVersion(ctx, tx, id, n, author, raw, files)
}

// withinQuota refuses, with ErrLimit and the reason, a write of size bytes that would take the
// database past the instance storage quota (004-AC7, 006-AC4).
func withinQuota(ctx context.Context, tx *txn, size int64) error {
	v, err := instanceSettings(ctx, tx)
	if err != nil || v.StorageQuota == 0 {
		return err
	}
	used, err := usedBytes(ctx, tx)
	if err != nil {
		return err
	}
	if used+size > v.StorageQuota {
		return fmt.Errorf("%w: the instance holds %s, and %s more would pass its %s storage quota", ErrLimit, mb(used), mb(size), mb(v.StorageQuota))
	}
	return nil
}

// DuplicateStarter copies the latest version of starter id, as profileID sees it, into a new
// personal starter of profileID (006-AC2).
func (s *Store) DuplicateStarter(ctx context.Context, profileID, id int64) (*Starter, error) {
	var dup int64
	// One transaction: a delete between the reads and the save cannot leave a copy without its blobs.
	err := s.withTx(ctx, "duplicate starter", func(tx *txn) error {
		st, m, err := starterVersion(ctx, tx, id, 0, profileID)
		if err != nil {
			return err
		}
		files, err := starterFiles(ctx, tx, id, st.Version)
		if err != nil {
			return err
		}
		dup, err = saveStarter(ctx, tx, profileID, 0, m, files)
		return err
	})
	if err != nil {
		return nil, err
	}
	st, _, err := s.StarterVersion(ctx, dup, 0, profileID)
	return st, err
}

// PublishStarter moves personal starter id of profileID, with its versions, to the instance
// library, audited (006-AC3, 006-AC5). ErrBuiltin, ErrForbidden, ErrNotFound.
func (s *Store) PublishStarter(ctx context.Context, profileID, id int64) error {
	return s.withTx(ctx, "publish starter", func(tx *txn) error {
		library, owner, err := starterOwner(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case library == LibraryBuiltin:
			return ErrBuiltin
		case owner != profileID || library != LibraryPersonal:
			return ErrForbidden
		}
		if _, err := tx.ExecContext(ctx, `UPDATE starters SET library = 'instance' WHERE id = ?`, id); err != nil {
			return fmt.Errorf("publish starter: %w", err)
		}
		return auditTx(ctx, tx, strconv.FormatInt(id, 10), map[string]any{"version": latestStarterVersion(ctx, tx, id)})
	})
}

// DeleteStarter deletes starter id with its versions: a personal one by its owner, an instance one
// by its author or, with profileID 0, the super-admin, audited (006-AC3, 006-AC5). Rooms created
// from it keep their copies. ErrBuiltin, ErrForbidden, ErrNotFound.
func (s *Store) DeleteStarter(ctx context.Context, profileID, id int64) error {
	return s.withTx(ctx, "delete starter", func(tx *txn) error {
		library, owner, err := starterOwner(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case library == LibraryBuiltin:
			return ErrBuiltin
		case owner != profileID && (profileID != 0 || library != LibraryInstance):
			return ErrForbidden
		}
		if library == LibraryInstance {
			if err := auditTx(ctx, tx, strconv.FormatInt(id, 10), map[string]any{"version": latestStarterVersion(ctx, tx, id)}); err != nil {
				return err
			}
		}
		return execOne(ctx, tx, `DELETE FROM starters WHERE id = ?`, id)
	})
}

func starterOwner(ctx context.Context, tx *txn, id int64) (library string, owner int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT library, COALESCE(profile_id, 0) FROM starters WHERE id = ?`, id).Scan(&library, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("starter %d: %w", id, err)
	}
	return library, owner, nil
}

func latestStarterVersion(ctx context.Context, tx *txn, id int64) int {
	var n int
	_ = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(n), 0) FROM starter_versions WHERE starter_id = ?`, id).Scan(&n)
	return n
}

// ensureStarters creates the built-in starters and adds a version to any whose shipped manifest
// changed since its latest version (006-AC2). Built-ins carry no blobs.
func ensureStarters(ctx context.Context, tx *txn) error {
	for _, b := range builtinStarters {
		raw, err := json.Marshal(b.m)
		if err != nil {
			return fmt.Errorf("built-in starter %s: %w", b.key, err)
		}
		if err := ensureStarter(ctx, tx, b.key, raw); err != nil {
			return fmt.Errorf("built-in starter %s: %w", b.key, err)
		}
	}
	return nil
}

func ensureStarter(ctx context.Context, tx *txn, key string, raw []byte) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO starters(builtin, library, created_at) VALUES(?, 'builtin', ?) ON CONFLICT(builtin) DO NOTHING`,
		key, now()); err != nil {
		return err
	}
	var id int64
	var n int
	var last string
	if err := tx.QueryRowContext(ctx, `SELECT s.id, COALESCE(v.n, 0), COALESCE(v.manifest, '') FROM starters s
		LEFT JOIN starter_versions v ON v.starter_id = s.id WHERE s.builtin = ? ORDER BY v.n DESC LIMIT 1`, key).Scan(&id, &n, &last); err != nil {
		return err
	}
	if n > 0 && last == string(raw) {
		return nil
	}
	return insertStarterVersion(ctx, tx, id, n+1, presetAuthor, raw, nil)
}

// BlankManifest is the blank starter: the Default template's persona alone, as every room had
// before 006. It is not stored (spec 006 Decisions).
func (s *Store) BlankManifest(ctx context.Context) (*StarterManifest, error) {
	t, err := scanTemplate(s.db.QueryRowContext(ctx, templateCols+`WHERE t.preset = ?`, PresetDefault))
	if err != nil {
		return nil, err
	}
	return &StarterManifest{Title: "Blank", Description: "One AI persona, " + t.Name + ", and nothing else.", Fields: []StarterField{},
		Personas: []StarterPersona{{PersonaFields: t.PersonaFields}}, Skills: []StarterSkill{}, Documents: []StarterDocument{},
		Chain: StarterChain{Budget: DefaultChainBudget}}, nil
}
