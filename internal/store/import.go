package store

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/convert"
	pluginpkg "github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/render"
	"github.com/babs/grill-room/internal/skill"
	"github.com/babs/grill-room/internal/turn"
	voicecat "github.com/babs/grill-room/internal/voices"
)

// ErrImportRefused wraps every reason an import is refused; the message says which. Nothing is
// created (004-AC14).
var ErrImportRefused = errors.New("import refused")

func refuseImport(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrImportRefused, fmt.Sprintf(format, args...))
}

// Bounds on what one imported row may hold: each is read into memory whole.
const (
	maxImportLine    = 1 << 20
	maxImportText    = 64 << 20
	maxImportSession = 128 << 20
)

// ImportOptions say who imports an export directory, as which new room, and within which limits.
type ImportOptions struct {
	// RoomID is the new room's id.
	RoomID string
	// Format is the archive's format: 1 holds no mentions.jsonl, 2 no plugins.
	Format int
	// Importer is the importing profile, 0 for the super-admin. A profile imports within the
	// room creation guardrails and becomes the room's admin, seated as DisplayName unless one of
	// the relinked participants is already its own.
	Importer    int64
	DisplayName string
	// SameInstance tells that this instance wrote the export: its shared and personal skills and
	// its templates are looked up by ref.
	SameInstance bool
	// SourceRoom is the id of the room exported, from the manifest. A profile importer relinks the
	// participants of that room only when it is that room's admin here: anyone else's uuid in an
	// archive is a claim, and would seat that profile in a room the importer controls.
	SourceRoom string
	// ArchiveBytes is the export's size uncompressed, which the instance storage quota counts.
	ArchiveBytes int64
	// DefaultQuota is the new room's quota on uploads and attachments; MaxUpload and
	// MaxAttachment bound one of each.
	DefaultQuota, MaxUpload, MaxAttachment int64
	// CheckPersona validates a persona as the room API would.
	CheckPersona func(p StarterPersona) error
	// DryRun runs the whole import and rolls it back: the preview (004-AC15).
	DryRun bool
}

// ImportReport is what an import created, or with DryRun would create.
type ImportReport struct {
	// Room is the new room; nil on a dry run.
	Room   *Room          `json:"room,omitempty"`
	Name   string         `json:"name"`
	Counts map[string]int `json:"counts"`
	// Bytes are the sizes by kind; uploads and attachments count against the room quota.
	Bytes map[string]int64 `json:"bytes"`
	// Relinked counts the participants linked to their profile by uuid; Unlinked those whose uuid
	// matches no profile here, kept as a hint. RoomSkills counts the embedded skills that land as
	// room skills; LinkedSkills those found in this instance's library.
	Relinked     int      `json:"relinked"`
	Unlinked     int      `json:"unlinked"`
	RoomSkills   int      `json:"room_skills"`
	LinkedSkills int      `json:"linked_skills"`
	Warnings     []string `json:"warnings"`
}

var errDryRun = errors.New("dry run")

// sessionIDRe is a Claude Code session id: daemons name the session's file after it.
var sessionIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// importer holds one import's state: the old refs mapped to new ids.
type importer struct {
	ctx  context.Context
	tx   *sql.Tx
	root *os.Root
	o    ImportOptions
	rep  *ImportReport
	room string

	participants, personas, uploads, documents, annotations, skills, plugins map[int64]int64
	// messages and annMessages keep refs and ids in insertion order, refs ascending, for the
	// resume cursors.
	messages, annMessages idMap
	// uploadFormat is the format each upload's content reads as.
	uploadFormat map[int64]string
	// relink holds the profile uuids a profile importer may relink; nil for the super-admin, who
	// relinks any.
	relink map[string]bool
	// versions are the skill versions imported, by new skill id.
	versions       map[int64][]int
	roomSkillNames map[string]bool
}

// idMap maps ascending refs to new ids.
type idMap struct{ refs, ids []int64 }

func (m *idMap) add(ref, id int64) error {
	if n := len(m.refs); n > 0 && ref <= m.refs[n-1] {
		return refuseImport("refs out of order: %d after %d", ref, m.refs[n-1])
	}
	m.refs, m.ids = append(m.refs, ref), append(m.ids, id)
	return nil
}

func (m *idMap) get(ref int64) (int64, bool) {
	i, ok := slices.BinarySearch(m.refs, ref)
	if !ok {
		return 0, false
	}
	return m.ids[i], true
}

// cursor maps a cursor: the new id of the last ref at or before it, 0 when none is.
func (m *idMap) cursor(ref int64) int64 {
	i, _ := slices.BinarySearch(m.refs, ref+1)
	if i == 0 {
		return 0
	}
	return m.ids[i-1]
}

// ImportRoom creates a new room from the export directory dir, whose files the caller checked
// against their manifest: new ids, the same content (004-AC12). Participants are relinked to the
// profile of their uuid here, or keep it as a hint (004-AC13); old invites are not carried, so
// none works. It runs in one transaction, guardrails and quotas included: a refusal creates
// nothing (004-AC14). ErrImportRefused, ErrCreationRefused.
// ponytail: the transaction holds the store's one connection for the whole import (seconds for a
// full room); stage into a second database file if imports ever need to run beside live rooms.
func (s *Store) ImportRoom(ctx context.Context, dir string, o ImportOptions) (*ImportReport, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("import room: %w", err)
	}
	defer root.Close() //nolint:errcheck // a directory handle: nothing to flush
	rep := &ImportReport{Counts: map[string]int{}, Bytes: map[string]int64{}, Warnings: []string{}}
	if err := renderAhead(ctx, root); err != nil {
		return nil, err
	}
	err = s.withTx(ctx, "import room", func(tx *sql.Tx) error {
		im := &importer{ctx: ctx, tx: tx, root: root, o: o, rep: rep, room: o.RoomID,
			participants: map[int64]int64{}, personas: map[int64]int64{}, uploads: map[int64]int64{}, documents: map[int64]int64{},
			annotations: map[int64]int64{}, skills: map[int64]int64{}, plugins: map[int64]int64{}, uploadFormat: map[int64]string{}, versions: map[int64][]int{},
			roomSkillNames: map[string]bool{}}
		if err := im.run(); err != nil {
			return err
		}
		if o.DryRun {
			return errDryRun
		}
		return auditTx(ctx, tx, o.RoomID, map[string]any{"counts": rep.Counts})
	})
	if errors.Is(err, errDryRun) {
		return rep, nil
	}
	if err != nil {
		return nil, err
	}
	if rep.Room, err = s.Room(ctx, o.RoomID); err != nil {
		return nil, err
	}
	return rep, nil
}

func (im *importer) run() error {
	var x xRoom
	b, err := im.read(fileRoom, maxImportText)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &x); err != nil {
		return refuseImport("%s: %v", fileRoom, err)
	}
	im.rep.Name = x.Name
	if err := im.guardrails(); err != nil {
		return err
	}
	if err := textIs("room name", maxImportRoomName, false, false, x.Name); err != nil {
		return err
	}
	if err := textIs("STT glossary", MaxSTTGlossary, true, true, x.STTGlossary); err != nil {
		return err
	}
	var from any
	if f := x.CreatedFrom; f != nil {
		if err := textIs("starter title", maxImportRoomName, false, false, f.Title); err != nil {
			return err
		}
		b, _ := json.Marshal(Provenance{Title: f.Title, Version: f.Version})
		from = string(b)
	}
	if x.Chain.Budget < 1 || x.Chain.Count < 0 || len(x.Personas) > MaxRoomPersonas {
		return refuseImport("%s: bad chain, or more than %d personas", fileRoom, MaxRoomPersonas)
	}
	sp := DefaultSpeech
	if x.Speech != nil {
		sp = *x.Speech
	}
	if !sp.Valid() {
		return refuseImport("call voice settings out of range: %+v", sp)
	}
	_, err = im.tx.ExecContext(im.ctx, `INSERT INTO rooms(id, name, created_at, creator_profile_id, chain_budget, chain_count, chain_suspended, parallel_turns, stt_glossary,
		created_from, voice_quiet_ms, voice_resume_s, voice_queue_s, voice_words) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, im.room, x.Name, now(), nullID(im.o.Importer),
		x.Chain.Budget, x.Chain.Count, x.Chain.Suspended, x.ParallelTurns, x.STTGlossary, from, sp.QuietMS, sp.ResumeS, sp.QueueS, sp.Words)
	if err != nil {
		return fmt.Errorf("import room: %w", err)
	}
	for _, step := range []func() error{
		func() error { return im.personasStep(x.Personas) },
		im.participantsStep, im.messagesStep, im.mentionsStep, im.documentsStep, im.annotationsStep, im.noteMentionsStep, im.skillsStep,
		func() error { return im.pinsAndResume(x.Personas) },
		func() error { return im.pluginsStep(x.Personas) },
		im.sessionsStep, im.seatStep,
	} {
		if err := step(); err != nil {
			return err
		}
	}
	// The imported lines are history, not new activity for the triggers (006-AC18).
	if _, err := im.tx.ExecContext(im.ctx, `UPDATE room_personas SET mark_msg_id = (SELECT COALESCE(MAX(id), 0) FROM messages WHERE room_id = ?),
		notes_mark = `+latestNote+` WHERE room_id = ?`, im.room, im.room); err != nil {
		return fmt.Errorf("import room: %w", err)
	}
	used, _, err := roomUsage(im.ctx, im.tx, im.room, im.o.DefaultQuota)
	if err != nil {
		return err
	}
	if used > im.o.DefaultQuota {
		return refuseImport("its uploads, attachments and plugins weigh %s, over the %s room quota here", mb(used), mb(im.o.DefaultQuota))
	}
	if im.rep.Counts["sessions"] > 0 {
		im.rep.Warnings = append(im.rep.Warnings, "The agent sessions hold everything Claude read in the room: whoever holds this archive can read it.")
	}
	if im.rep.Unlinked > 0 {
		im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("Participants with no profile on this instance: %d. They keep their profile uuid, unlinked.", im.rep.Unlinked))
	}
	if n := im.rep.Counts["plugins"]; n > 0 {
		im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("Plugins: %d. None loads until this instance's AI host checks and approves it.", n))
	}
	if im.rep.RoomSkills > 0 {
		im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("Shared or personal skills landing as room skills: %d.", im.rep.RoomSkills))
	}
	return nil
}

// ValidText reports whether s is text the room takes: UTF-8 of at most maxLen characters, with no
// control character but newlines and tabs when multiline, and no bidi control when not.
func ValidText(s string, maxLen int, multiline bool) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxLen {
		return false
	}
	for _, c := range s {
		if multiline && (c == '\n' || c == '\t') {
			continue
		}
		if unicode.IsControl(c) || (!multiline && unicode.Is(unicode.Bidi_Control, c)) {
			return false
		}
	}
	return true
}

// Bounds on imported text, as the room API sets them: an archive brings nothing a live room
// would refuse.
const (
	maxImportRoomName = 100
	maxImportName     = 100
	maxImportName40   = 40
	maxImportNote     = turn.MaxModelNote
	maxImportLineBody = 4000
	maxImportAgent    = 20000
	maxImportDocName  = 200
	maxImportDocText  = 2 << 20
)

// textIs refuses what, unless every one of texts is valid text of at most maxLen characters, and
// non-empty unless empty allows it.
func textIs(what string, maxLen int, multiline, empty bool, texts ...string) error {
	for _, s := range texts {
		if (s == "" && !empty) || !ValidText(s, maxLen, multiline) {
			return refuseImport("%s: text too long, empty or holding control characters", what)
		}
	}
	return nil
}

// guardrails applies the room creation limits to a profile, and the instance storage quota to the
// archive's size, whoever imports (004-AC7, 004-AC14).
func (im *importer) guardrails() error {
	v, err := instanceSettings(im.ctx, im.tx)
	if err != nil {
		return err
	}
	if im.o.Importer != 0 {
		if err := creationAllowed(im.ctx, im.tx, v, im.o.Importer); err != nil {
			return err
		}
	}
	if v.StorageQuota > 0 {
		used, err := usedBytes(im.ctx, im.tx)
		if err != nil {
			return err
		}
		if used+im.o.ArchiveBytes > v.StorageQuota {
			return refuseImport("the instance holds %s and the archive %s, over its %s storage quota", mb(used), mb(im.o.ArchiveBytes), mb(v.StorageQuota))
		}
	}
	return nil
}

// read reads file name of the export, refused past limit bytes.
func (im *importer) read(name string, limit int64) ([]byte, error) {
	f, err := im.root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, refuseImport("%s is missing", name)
	}
	if err != nil {
		return nil, refuseImport("%s: %v", name, err)
	}
	defer f.Close() //nolint:errcheck // read only
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("import %s: %w", name, err)
	}
	if int64(len(b)) > limit {
		return nil, refuseImport("%s is over %s, too large for this instance", name, mb(limit))
	}
	return b, nil
}

// eachLine decodes JSON Lines file name, one value at a time, into fn.
func eachLine[T any](im *importer, name string, fn func(*T) error) error {
	f, err := im.root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return refuseImport("%s is missing", name)
	}
	if err != nil {
		return refuseImport("%s: %v", name, err)
	}
	defer f.Close() //nolint:errcheck // read only
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxImportLine)
	for n := 1; sc.Scan(); n++ {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return refuseImport("%s line %d: %v", name, n, err)
		}
		if err := fn(&v); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return refuseImport("%s: %v", name, err)
	}
	return nil
}

// ref maps a reference of what to its new id; 0 stays 0 when optional.
func ref(m map[int64]int64, what string, r int64, optional bool) (any, error) {
	if r == 0 && optional {
		return nil, nil
	}
	id, ok := m[r]
	if !ok {
		return nil, refuseImport("unknown %s %d", what, r)
	}
	return id, nil
}

func (im *importer) personasStep(ps []xPersona) error {
	for _, p := range ps {
		// An archive from before 005-AC9 may leave either to the host, as a migrated room did.
		if p.Model == "" {
			p.Model = DefaultModel
		}
		if p.Effort == "" {
			p.Effort = DefaultEffort
		}
		// An unknown voice falls back to the default rather than refuse the room; a rate out of
		// range is refused with the API's words.
		if err := im.o.CheckPersona(StarterPersona{PersonaFields: p.PersonaFields, EveryN: p.EveryN, EveryVoice: p.EveryVoice, EveryMinutes: p.EveryMinutes,
			Autocompact: p.Autocompact, VoiceRate: p.VoiceRate}); err != nil {
			return refuseImport("persona %s: %v", p.Name, err)
		}
		if p.WatchesNotes && slices.ContainsFunc(ps, func(o xPersona) bool { return o.WatchesNotes && o.Ref < p.Ref }) {
			return refuseImport("persona %s: another persona watches the notes already", p.Name)
		}
		tpl, err := im.template(p)
		if err != nil {
			return err
		}
		var id int64
		err = im.tx.QueryRowContext(im.ctx, `INSERT INTO room_personas(room_id, template_id, template_version, name, badge, colour, text, model, effort,
			every_n, self_call, autocompact, created_at, every_voice, every_minutes, watches_notes, speaks, voice, voice_rate, mark_msg_id, mark_at_ms)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,`+markNow+`) RETURNING id`,
			append(append([]any{im.room, tpl, p.TemplateVersion}, p.PersonaFields.args()...), nullID(int64(p.EveryN)), p.SelfCall, p.Autocompact, p.CreatedAt,
				nullInt(p.EveryVoice), nullInt(p.EveryMinutes), p.WatchesNotes, p.Speaks == nil || *p.Speaks, voicecat.Known(p.Voice),
				cmp.Or(p.VoiceRate, 1), time.Now().UnixMilli())...).Scan(&id)
		if err != nil {
			return refuseImport("persona %s: %v", p.Name, err)
		}
		if _, dup := im.personas[p.Ref]; dup {
			return refuseImport("persona %d twice", p.Ref)
		}
		im.personas[p.Ref] = id
	}
	im.rep.Counts["personas"] = len(ps)
	return nil
}

// template is the library template of persona p here: its own on the same instance, the preset
// of the same key anywhere, else none.
func (im *importer) template(p xPersona) (any, error) {
	var id int64
	var err error
	switch {
	case p.TemplatePreset != "":
		err = im.tx.QueryRowContext(im.ctx, `SELECT id FROM persona_templates WHERE preset = ?`, p.TemplatePreset).Scan(&id)
	case p.Template != 0 && im.o.SameInstance:
		err = im.tx.QueryRowContext(im.ctx, `SELECT id FROM persona_templates WHERE id = ?`, p.Template).Scan(&id)
	default:
		return nil, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("import persona template: %w", err)
	}
	return id, nil
}

// newHash is the hash of a fresh random token nobody holds: invites and seats of imported
// participants open nothing by themselves.
func newHash() string {
	h := sha256.Sum256([]byte(rand.Text()))
	return hex.EncodeToString(h[:])
}

// invite creates one of the new room's invites, revoked or not.
func (im *importer) invite(revoked bool) (int64, error) {
	var at any
	if revoked {
		at = now()
	}
	var id int64
	err := im.tx.QueryRowContext(im.ctx, `INSERT INTO invites(room_id, token_hash, created_at, revoked_at) VALUES(?,?,?,?) RETURNING id`,
		im.room, newHash(), now(), at).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("import invite: %w", err)
	}
	return id, nil
}

// relinkSet fills relink for a profile importer: its own uuid, and the profiles of the source
// room's participants when it administers that room here.
func (im *importer) relinkSet() error {
	if im.o.Importer == 0 {
		return nil
	}
	im.relink = map[string]bool{}
	rows, err := im.tx.QueryContext(im.ctx, `SELECT uuid FROM profiles WHERE id = ? AND uuid IS NOT NULL
		UNION SELECT f.uuid FROM participants p JOIN profiles f ON f.id = p.profile_id
		WHERE p.room_id = ? AND f.uuid IS NOT NULL AND EXISTS (SELECT 1 FROM room_admins a WHERE a.room_id = p.room_id AND a.profile_id = ?)`,
		im.o.Importer, im.o.SourceRoom, im.o.Importer)
	if err != nil {
		return fmt.Errorf("import relink: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return fmt.Errorf("import relink: %w", err)
		}
		im.relink[uuid] = true
	}
	return rows.Err()
}

func (im *importer) relinkable(uuid string) bool { return im.relink == nil || im.relink[uuid] }

// participantsStep seats every participant under one invite nobody holds: the active ones under a
// valid invite, those whose invite was revoked under a revoked one. Each is relinked to the profile
// of its uuid when there is one and the importer may relink it, and a relinked room admin stays
// one. Any other keeps its uuid as a hint.
func (im *importer) participantsStep() error {
	if err := im.relinkSet(); err != nil {
		return err
	}
	invites := map[bool]int64{}
	err := eachLine(im, fileParticipants, func(p *xParticipant) error {
		inv, ok := invites[p.Active]
		if !ok {
			var err error
			if inv, err = im.invite(!p.Active); err != nil {
				return err
			}
			invites[p.Active] = inv
		}
		if err := textIs(fmt.Sprintf("participant %d name", p.Ref), maxImportName40, false, false, p.Name); err != nil {
			return err
		}
		if err := textIs(fmt.Sprintf("participant %d previous name", p.Ref), maxImportName40, false, true, p.PreviousName); err != nil {
			return err
		}
		var profile, hint any
		if p.ProfileUUID != "" && !im.relinkable(p.ProfileUUID) {
			hint = p.ProfileUUID
			im.rep.Unlinked++
		} else if p.ProfileUUID != "" {
			var id int64
			err := im.tx.QueryRowContext(im.ctx, `SELECT id FROM profiles WHERE uuid = ?`, p.ProfileUUID).Scan(&id)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				hint = p.ProfileUUID
				im.rep.Unlinked++
			case err != nil:
				return fmt.Errorf("import participant: %w", err)
			default:
				profile = id
				im.rep.Relinked++
			}
		}
		var id int64
		err := im.tx.QueryRowContext(im.ctx, `INSERT INTO participants(room_id, invite_id, name, token_hash, joined_at, can_trigger, profile_id, profile_uuid_hint, previous_name)
			VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, im.room, inv, p.Name, newHash(), p.JoinedAt, p.CanTrigger, profile, hint, nullStr(p.PreviousName)).Scan(&id)
		if err != nil {
			return refuseImport("participant %d: %v", p.Ref, err)
		}
		if _, dup := im.participants[p.Ref]; dup {
			return refuseImport("participant %d twice", p.Ref)
		}
		if p.Admin && p.Active && profile != nil {
			if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES(?,?,?) ON CONFLICT DO NOTHING`,
				im.room, profile, now()); err != nil {
				return fmt.Errorf("import room admin: %w", err)
			}
		}
		im.participants[p.Ref] = id
		im.rep.Counts["participants"]++
		return nil
	})
	return err
}

// fileRendered holds each line's HTML, rendered before the import's transaction: a line takes up to
// a tenth of a second, and the transaction holds the store's only connection. No archive path starts
// with a dot.
const fileRendered = ".rendered.jsonl"

// checkMessage refuses a line the room API would not take.
func checkMessage(m *xMessage) error {
	limit := maxImportLineBody
	if m.Kind == KindAgent || m.Kind == KindSystem {
		limit = maxImportAgent
	}
	what := fmt.Sprintf("message %d", m.Ref)
	if err := textIs(what, limit, true, false, m.Body); err != nil {
		return err
	}
	if err := textIs(what, maxImportName, false, false, m.Sender); err != nil {
		return err
	}
	if err := textIs(what, maxImportName, false, true, m.Model, m.Effort, m.ReqModel, m.ReqEffort); err != nil {
		return err
	}
	switch {
	case m.Kind != KindVoice && m.Source != "":
		return refuseImport("%s: source on a %q line", what, m.Kind)
	case m.Kind == KindVoice && m.Source != "" && m.Source != VoiceSourceDevice && m.Source != VoiceSourceServer:
		return refuseImport("%s: unknown source %q", what, m.Source)
	}
	return textIs(what, maxImportNote, false, true, m.ModelNote)
}

// importSource is the source a checked line is stored with: an archive from before 010 names none,
// and its voice lines came from the server's worker (010-AC11).
func importSource(m *xMessage) any {
	if m.Kind == KindVoice && m.Source == "" {
		return VoiceSourceServer
	}
	return nullStr(m.Source)
}

// renderAhead checks every line and writes its HTML to fileRendered, one JSON value per line in
// the order of the lines: never taken from the archive, as the room shows it as HTML.
func renderAhead(ctx context.Context, root *os.Root) error {
	f, err := root.Create(fileRendered)
	if err != nil {
		return fmt.Errorf("import render: %w", err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	err = eachLine(&importer{ctx: ctx, root: root}, fileMessages, func(m *xMessage) error {
		if err := checkMessage(m); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var html *string
		if m.Kind != KindSystem {
			h := render.MessageHTML(m.Body)
			html = &h
		}
		return enc.Encode(html)
	})
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (im *importer) messagesStep() error {
	rf, err := im.root.Open(fileRendered)
	if err != nil {
		return fmt.Errorf("import messages: %w", err)
	}
	defer rf.Close() //nolint:errcheck // read only
	rendered := json.NewDecoder(bufio.NewReader(rf))
	err = eachLine(im, fileMessages, func(m *xMessage) error {
		var html *string
		if err := rendered.Decode(&html); err != nil {
			return fmt.Errorf("import messages: %w", err)
		}
		pid, err := ref(im.participants, "participant", m.Participant, true)
		if err != nil {
			return err
		}
		persona, err := ref(im.personas, "persona", m.Persona, true)
		if err != nil {
			return err
		}
		var id int64
		err = im.tx.QueryRowContext(im.ctx, `INSERT INTO messages(room_id, ts, kind, participant_id, sender, body, html, model, effort, persona_id,
			req_model, req_effort, model_note, model_mismatch, edited_at, deleted_at, change_seq, source) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
			im.room, m.TS, m.Kind, pid, m.Sender, m.Body, html, m.Model, m.Effort, persona, m.ReqModel, m.ReqEffort, m.ModelNote, m.Mismatch,
			m.EditedAt, m.DeletedAt, m.ChangeSeq, importSource(m)).Scan(&id)
		if err != nil {
			return refuseImport("message %d: %v", m.Ref, err)
		}
		im.rep.Counts["messages"]++
		return im.messages.add(m.Ref, id)
	})
	if err != nil {
		return err
	}
	return eachLine(im, fileMessageVersions, func(v *xMessageVersion) error {
		id, ok := im.messages.get(v.Message)
		if !ok {
			return refuseImport("message version of unknown message %d", v.Message)
		}
		if err := textIs(fmt.Sprintf("version of message %d", v.Message), maxImportAgent, true, false, v.Body); err != nil {
			return err
		}
		if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO message_versions(message_id, seq, body, written_at) VALUES(?,?,?,?)`,
			id, v.Seq, v.Body, v.WrittenAt); err != nil {
			return fmt.Errorf("import message version: %w", err)
		}
		im.rep.Counts["message_versions"]++
		return nil
	})
}

// blob reads the blob file named by a row, refused past limit; what names the row for errors. The
// export directory is an os.Root: a name cannot leave it.
// mentionsStep links the imported lines to how their mentions' turns ended (005-AC4). The turn
// history stays behind, so a turn still queued or running at export never ends here: cancelled.
func (im *importer) mentionsStep() error {
	if im.o.Format < 2 {
		return nil
	}
	return im.linkMentions(fileMentions, "message_turns", "mentions", im.messages.get)
}

// noteMentionsStep does the same for the note messages (009-AC29). The file is optional in format
// 2: an archive from before 009 has none.
func (im *importer) noteMentionsStep() error {
	if _, err := im.root.Stat(fileNoteMentions); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return im.linkMentions(fileNoteMentions, "annotation_message_turns", "note_mentions", im.annMessages.get)
}

// linkMentions reads file into table, its message refs mapped by message.
func (im *importer) linkMentions(file, table, count string, message func(int64) (int64, bool)) error {
	final := map[string]string{"done": "done", "failed": "failed", "stopped": "stopped", "cancelled": MentionCancelled,
		"queued": MentionCancelled, "running": MentionCancelled}
	return eachLine(im, file, func(x *xMention) error {
		msg, ok := message(x.Message)
		status, known := final[x.Status]
		persona, isPersona := im.personas[x.Persona]
		if !ok || !known || !isPersona {
			return refuseImport("mention of message %d: unknown message, persona or status %q", x.Message, x.Status)
		}
		var line any
		if x.Line != 0 {
			id, ok := im.messages.get(x.Line)
			if !ok {
				return refuseImport("mention of message %d: unknown line %d", x.Message, x.Line)
			}
			line = id
		}
		//nolint:gosec // table is one of mentionTables, never user input
		_, err := im.tx.ExecContext(im.ctx, `INSERT INTO `+table+`(message_id, persona_id, final_status, final_line_id) VALUES(?,?,?,?)`,
			msg, persona, status, line)
		if err != nil {
			return refuseImport("mention of message %d: %v", x.Message, err)
		}
		im.rep.Counts[count]++
		return nil
	})
}

func (im *importer) blob(name, what string, limit int64) ([]byte, error) {
	if name == "" {
		return nil, refuseImport("%s names no file", what)
	}
	return im.read(name, limit)
}

func (im *importer) documentsStep() error {
	err := eachLine(im, fileUploads, func(u *xUpload) error {
		if err := textIs(fmt.Sprintf("upload %d", u.Ref), maxImportDocName, false, false, u.Filename); err != nil {
			return err
		}
		pid, err := ref(im.participants, "participant", u.Participant, true)
		if err != nil {
			return err
		}
		data, err := im.blob(u.File, "upload "+strconv.FormatInt(u.Ref, 10), im.o.MaxUpload)
		if err != nil {
			return err
		}
		// The type is read from the content, as for an upload: the archive's word decides nothing.
		typ, err := convert.Detect(u.Filename, data)
		if err != nil {
			return refuseImport("upload %s: %v", u.Filename, err)
		}
		var id int64
		err = im.tx.QueryRowContext(im.ctx, `INSERT INTO uploads(room_id, participant_id, filename, content_type, size, data, created_at)
			VALUES(?,?,?,?,?,?,?) RETURNING id`, im.room, pid, u.Filename, typ.ContentType, len(data), data, u.CreatedAt).Scan(&id)
		if err != nil {
			return refuseImport("upload %d: %v", u.Ref, err)
		}
		if _, dup := im.uploads[u.Ref]; dup {
			return refuseImport("upload %d twice", u.Ref)
		}
		im.uploads[u.Ref], im.uploadFormat[u.Ref] = id, typ.Format
		im.rep.Counts["uploads"]++
		im.rep.Bytes["uploads"] += int64(len(data))
		return nil
	})
	if err != nil {
		return err
	}
	err = eachLine(im, fileDocuments, func(d *xDocument) error {
		if err := textIs(fmt.Sprintf("document %d", d.Ref), maxImportDocName, false, false, d.Name, d.Author); err != nil {
			return err
		}
		up, err := ref(im.uploads, "upload", d.Upload, true)
		if err != nil {
			return err
		}
		if d.Upload != 0 && im.uploadFormat[d.Upload] != d.Format {
			return refuseImport("document %s: format %s, but its upload reads as %s", d.Name, d.Format, im.uploadFormat[d.Upload])
		}
		var deleted *string
		if d.DeletedAt != "" {
			if _, err := time.Parse(time.RFC3339Nano, d.DeletedAt); err != nil {
				return refuseImport("document %d: deleted_at %q is not a time", d.Ref, d.DeletedAt)
			}
			deleted = &d.DeletedAt
		}
		var id int64
		err = im.tx.QueryRowContext(im.ctx, `INSERT INTO documents(room_id, kind, name, format, upload_id, author, created_at, deleted_at)
			VALUES(?,?,?,?,?,?,?,?) RETURNING id`, im.room, d.Kind, d.Name, d.Format, up, d.Author, d.CreatedAt, deleted).Scan(&id)
		if err != nil {
			return refuseImport("document %d: %v", d.Ref, err)
		}
		if _, dup := im.documents[d.Ref]; dup {
			return refuseImport("document %d twice", d.Ref)
		}
		im.documents[d.Ref] = id
		im.rep.Counts["documents"]++
		return nil
	})
	if err != nil {
		return err
	}
	return eachLine(im, fileRevisions, func(r *xRevision) error {
		doc, err := ref(im.documents, "document", r.Document, false)
		if err != nil {
			return err
		}
		body, err := im.blob(r.File, "revision", maxImportText)
		if err != nil {
			return err
		}
		if err := textIs(fmt.Sprintf("revision %d of document %d", r.N, r.Document), maxImportName, false, false, r.Author); err != nil {
			return err
		}
		if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO revisions(document_id, n, body, author, created_at) VALUES(?,?,?,?,?)`,
			doc, r.N, string(body), r.Author, r.CreatedAt); err != nil {
			return refuseImport("revision %d of document %d: %v", r.N, r.Document, err)
		}
		im.rep.Counts["revisions"]++
		im.rep.Bytes["revisions"] += int64(len(body))
		return nil
	})
}

func (im *importer) annotationsStep() error {
	err := eachLine(im, fileAnnotations, func(a *xAnnotation) error {
		doc, err := ref(im.documents, "document", a.Document, false)
		if err != nil {
			return err
		}
		var anc any
		if len(a.Anchor) > 0 && string(a.Anchor) != "null" {
			var v anchor.Anchor
			if err := json.Unmarshal(a.Anchor, &v); err != nil {
				return refuseImport("annotation %d anchor: %v", a.Ref, err)
			}
			if err := v.Validate(); err != nil {
				return refuseImport("annotation %d anchor: %v", a.Ref, err)
			}
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("import annotation: %w", err)
			}
			anc = string(b)
		}
		var id int64
		err = im.tx.QueryRowContext(im.ctx, `INSERT INTO annotations(room_id, document_id, revision, anchor, orphaned, status, version, created_at)
			VALUES(?,?,?,?,?,?,?,?) RETURNING id`, im.room, doc, a.Revision, anc, a.Orphaned, a.Status, a.Version, a.CreatedAt).Scan(&id)
		if err != nil {
			return refuseImport("annotation %d: %v", a.Ref, err)
		}
		if _, dup := im.annotations[a.Ref]; dup {
			return refuseImport("annotation %d twice", a.Ref)
		}
		im.annotations[a.Ref] = id
		im.rep.Counts["annotations"]++
		return nil
	})
	if err != nil {
		return err
	}
	err = eachLine(im, fileAnnotationMessages, func(m *xAnnotationMessage) error {
		what := fmt.Sprintf("annotation message %d", m.Ref)
		if err := textIs(what, maxImportLineBody, true, true, m.Body); err != nil {
			return err
		}
		if err := textIs(what, maxImportName, false, false, m.Author); err != nil {
			return err
		}
		ann, err := ref(im.annotations, "annotation", m.Annotation, false)
		if err != nil {
			return err
		}
		pid, err := ref(im.participants, "participant", m.Participant, true)
		if err != nil {
			return err
		}
		persona, err := ref(im.personas, "persona", m.Persona, true)
		if err != nil {
			return err
		}
		var id int64
		err = im.tx.QueryRowContext(im.ctx, `INSERT INTO annotation_messages(annotation_id, author_kind, participant_id, persona_id, author, body, created_at)
			VALUES(?,?,?,?,?,?,?) RETURNING id`, ann, m.By, pid, persona, m.Author, m.Body, m.CreatedAt).Scan(&id)
		if err != nil {
			return refuseImport("annotation message %d: %v", m.Ref, err)
		}
		im.rep.Counts["annotation_messages"]++
		return im.annMessages.add(m.Ref, id)
	})
	if err != nil {
		return err
	}
	return eachLine(im, fileAttachments, func(a *xAttachment) error {
		msg, ok := im.annMessages.get(a.Message)
		if !ok {
			return refuseImport("attachment %d of unknown annotation message %d", a.Ref, a.Message)
		}
		data, err := im.blob(a.File, "attachment "+strconv.FormatInt(a.Ref, 10), im.o.MaxAttachment)
		if err != nil {
			return err
		}
		ct, ok := convert.Image(data)
		if !ok {
			return refuseImport("attachment %d is not a PNG, JPEG, GIF or WebP image", a.Ref)
		}
		if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO attachments(message_id, content_type, size, data, created_at) VALUES(?,?,?,?,?)`,
			msg, ct, len(data), data, a.CreatedAt); err != nil {
			return fmt.Errorf("import attachment: %w", err)
		}
		im.rep.Counts["attachments"]++
		im.rep.Bytes["attachments"] += int64(len(data))
		return nil
	})
}

// skillsStep imports the room's skills, and links each embedded shared or personal skill to its
// original when this instance wrote the export and still holds every embedded version unchanged.
// Any other lands as a room skill, renamed if the room has one of that name (004-AC13).
func (im *importer) skillsStep() error {
	type embedded struct {
		x        xSkill
		versions []xSkillVersion
	}
	var list []*embedded
	byRef := map[int64]*embedded{}
	rooms := 0
	err := eachLine(im, fileSkills, func(k *xSkill) error {
		if _, dup := byRef[k.Ref]; dup {
			return refuseImport("skill %d twice", k.Ref)
		}
		if !skill.ValidName(k.Name) {
			return refuseImport("skill %d: %q is not a skill name", k.Ref, k.Name)
		}
		if k.Scope == ScopeRoom {
			rooms++
		}
		// Embedded skills are those personas pin: no more than every persona pinning its most.
		if rooms > MaxRoomSkills || len(list) >= MaxRoomSkills+MaxRoomPersonas*MaxPersonaSkills {
			return refuseImport("too many skills: at most %d of the room's own", MaxRoomSkills)
		}
		e := &embedded{x: *k}
		list, byRef[k.Ref] = append(list, e), e
		return nil
	})
	if err != nil {
		return err
	}
	err = eachLine(im, fileSkillVersions, func(v *xSkillVersion) error {
		e, ok := byRef[v.Skill]
		if !ok {
			return refuseImport("version of unknown skill %d", v.Skill)
		}
		if len(e.versions) == MaxSkillVersions || slices.ContainsFunc(e.versions, func(w xSkillVersion) bool { return w.N == v.N }) {
			return refuseImport("skill %s: a version twice, or more than %d versions", e.x.Name, MaxSkillVersions)
		}
		if err := textIs("skill "+e.x.Name, skill.MaxDescription, true, false, v.Description); err != nil {
			return err
		}
		if err := textIs("skill "+e.x.Name, maxImportName, false, false, v.Author); err != nil {
			return err
		}
		e.versions = append(e.versions, *v)
		return nil
	})
	if err != nil {
		return err
	}
	for _, e := range list {
		if e.x.Scope == ScopeRoom {
			im.roomSkillNames[e.x.Name] = true
		}
	}
	for _, e := range list {
		if len(e.versions) == 0 {
			return refuseImport("skill %s has no version", e.x.Name)
		}
		im.rep.Counts["skills"]++
		if id, ok, err := im.linkSkill(e.x, e.versions); err != nil {
			return err
		} else if ok {
			im.skills[e.x.Ref] = id
			im.rep.LinkedSkills++
			continue
		}
		name := e.x.Name
		if e.x.Scope != ScopeRoom {
			for n := 2; im.roomSkillNames[name]; n++ {
				name = e.x.Name + "-" + strconv.Itoa(n)
			}
			im.roomSkillNames[name] = true
			im.rep.RoomSkills++
		}
		var id int64
		err := im.tx.QueryRowContext(im.ctx, `INSERT INTO skills(name, created_at, room_id, pending) VALUES(?,?,?,?) RETURNING id`,
			name, e.x.CreatedAt, im.room, e.x.Pending && e.x.Scope == ScopeRoom).Scan(&id)
		if err != nil {
			return refuseImport("skill %s: %v", e.x.Name, err)
		}
		im.skills[e.x.Ref] = id
		for _, v := range e.versions {
			if err := im.skillVersion(id, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// linkSkill finds embedded skill k in this instance's library: same instance, same scope and name,
// every version present with the hash of the archive's own files. A skill is linked only to content
// the archive itself carries, so no import reaches a skill its maker could not read.
func (im *importer) linkSkill(k xSkill, versions []xSkillVersion) (int64, bool, error) {
	if !im.o.SameInstance || (k.Scope != ScopeShared && k.Scope != ScopePersonal) {
		return 0, false, nil
	}
	var name, scope string
	err := im.tx.QueryRowContext(im.ctx, `SELECT name, scope FROM skills WHERE id = ?`, k.Ref).Scan(&name, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("import skill: %w", err)
	}
	if name != k.Name || scope != k.Scope {
		return 0, false, nil
	}
	for _, v := range versions {
		var sha string
		err := im.tx.QueryRowContext(im.ctx, `SELECT sha256 FROM skill_versions WHERE skill_id = ? AND n = ?`, k.Ref, v.N).Scan(&sha)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, fmt.Errorf("import skill: %w", err)
		}
		// The archive's own bytes, not its word: a declared hash alone would reach a skill whose
		// files the importer never had.
		data, err := im.blob(v.File, "skill version", skill.MaxZipBytes)
		if err != nil {
			return 0, false, err
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != sha {
			return 0, false, nil
		}
	}
	for _, v := range versions {
		im.versions[k.Ref] = append(im.versions[k.Ref], v.N)
	}
	return k.Ref, true, nil
}

// skillVersion stores version v of new skill id, its files validated as an upload's are.
func (im *importer) skillVersion(id int64, v xSkillVersion) error {
	data, err := im.blob(v.File, "skill version", skill.MaxZipBytes)
	if err != nil {
		return err
	}
	sk, err := skill.ParseTar(data)
	if err != nil {
		return refuseImport("skill version %d of %d: %v", v.N, v.Skill, err)
	}
	if sk.SHA256 != v.SHA256 {
		return refuseImport("skill version %d of %d: its files do not match its sha256", v.N, v.Skill)
	}
	if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO skill_versions(skill_id, n, author, description, files, size, sha256, data, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, id, v.N, v.Author, sk.Description, sk.Files, sk.Size, sk.SHA256, data, v.CreatedAt); err != nil {
		return refuseImport("skill version %d of %d: %v", v.N, v.Skill, err)
	}
	im.versions[id] = append(im.versions[id], v.N)
	im.rep.Counts["skill_versions"]++
	im.rep.Bytes["skills"] += int64(len(data))
	return nil
}

// pinsAndResume turns each persona's skills back on, at the same versions, and records where its
// next turn picks up: a done turn holding its session and cursors, mapped to the new ids.
func (im *importer) pinsAndResume(ps []xPersona) error {
	for _, p := range ps {
		persona := im.personas[p.Ref]
		if len(p.Skills) > MaxPersonaSkills {
			return refuseImport("persona %s has more than %d skills on", p.Name, MaxPersonaSkills)
		}
		for _, k := range p.Skills {
			id, ok := im.skills[k.Skill]
			if !ok || !slices.Contains(im.versions[id], k.Version) {
				return refuseImport("persona %s pins skill %d v%d, which the archive does not hold", p.Name, k.Skill, k.Version)
			}
			if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO room_persona_skills(persona_id, skill_id, version) VALUES(?,?,?)`, persona, id, k.Version); err != nil {
				return refuseImport("persona %s skill pin: %v", p.Name, err)
			}
		}
		r := p.Resume
		if r == nil {
			continue
		}
		if !sessionIDRe.MatchString(r.SessionID) {
			return refuseImport("persona %s resumes %q, which is not a session id", p.Name, r.SessionID)
		}
		ended := r.EndedAt
		if ended == "" {
			ended = now()
		}
		if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO turns(room_id, host, status, fresh, resume, session_id, message_cursor, annotation_cursor,
			change_cursor, prompt, started_at, ended_at, persona_id, persona_name, model, effort, cause)
			VALUES(?, '', 'done', 0, '', ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, 'imported')`,
			im.room, r.SessionID, im.messages.cursor(r.MessageCursor), im.annMessages.cursor(r.AnnotationCursor), r.ChangeCursor,
			ended, ended, persona, p.Name, r.Model, r.Effort); err != nil {
			return fmt.Errorf("import resume point: %w", err)
		}
	}
	return nil
}

func (im *importer) sessionsStep() error {
	return eachLine(im, fileSessions, func(x *xSession) error {
		persona, err := ref(im.personas, "persona", x.Persona, false)
		if err != nil {
			return err
		}
		data, err := im.blob(x.File, "session "+x.SessionID, maxImportSession)
		if err != nil {
			return err
		}
		if !sessionIDRe.MatchString(x.SessionID) {
			return refuseImport("session of persona %d: %q is not a session id", x.Persona, x.SessionID)
		}
		if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO agent_sessions(room_id, persona_id, session_id, data, updated_at, context_tokens, context_window, context_updated_at)
			VALUES(?,?,?,?,?,?,?,?)`, im.room, persona, x.SessionID, data, x.UpdatedAt, x.ContextTokens, x.ContextWindow, x.ContextUpdatedAt); err != nil {
			return refuseImport("session %s: %v", x.SessionID, err)
		}
		im.rep.Counts["sessions"]++
		im.rep.Bytes["sessions"] += int64(len(data))
		return nil
	})
}

// seatStep makes a profile importer the room's admin, and seats it unless one of the relinked
// participants still in the room is its own.
func (im *importer) seatStep() error {
	if im.o.Importer == 0 {
		return nil
	}
	var seated bool
	err := im.tx.QueryRowContext(im.ctx, `SELECT EXISTS (SELECT 1 FROM participants p JOIN invites i ON i.id = p.invite_id
		WHERE p.room_id = ? AND p.profile_id = ? AND i.revoked_at IS NULL)`, im.room, im.o.Importer).Scan(&seated)
	if err != nil {
		return fmt.Errorf("import seat: %w", err)
	}
	if !seated {
		if im.o.DisplayName == "" {
			return refuseImport("a display name is needed: you are not one of the room's participants")
		}
		inv, err := im.invite(false)
		if err != nil {
			return err
		}
		var pid int64
		if err := im.tx.QueryRowContext(im.ctx, `INSERT INTO participants(room_id, invite_id, name, token_hash, joined_at, profile_id) VALUES(?,?,?,?,?,?) RETURNING id`,
			im.room, inv, im.o.DisplayName, newHash(), now(), im.o.Importer).Scan(&pid); err != nil {
			return fmt.Errorf("import seat: %w", err)
		}
		// The importer's own seat, as a room creator's: revoking it lets them go like any other.
		if _, err := im.tx.ExecContext(im.ctx, `UPDATE invites SET created_by = ? WHERE id = ?`, pid, inv); err != nil {
			return fmt.Errorf("import seat: %w", err)
		}
	}
	if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES(?,?,?) ON CONFLICT DO NOTHING`,
		im.room, im.o.Importer, now()); err != nil {
		return fmt.Errorf("import room admin: %w", err)
	}
	return nil
}

// pluginCommitRe is a git commit id, as the server pins it; sha256Re a snapshot's hash.
var (
	pluginCommitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Re       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// supersededReason is why a version whose snapshot stayed behind never loads here.
const supersededReason = "superseded before the export: its files did not travel"

// pluginsStep imports the room's plugins pinned at their versions, and turns them on for the
// personas that had them (009-AC27). A version with its snapshot waits for this instance's AI
// host: inspected again, then held for its approval whatever the mode, the archive's report and
// decision set aside. One without, superseded before the export, stays a record that never loads.
// Every snapshot is checked as a daemon's upload is, against the instance's cap. Format 2 and
// older carry none.
func (im *importer) pluginsStep(ps []xPersona) error {
	if im.o.Format < 3 {
		return nil
	}
	set, err := instanceSettings(im.ctx, im.tx)
	if err != nil {
		return err
	}
	maxBytes := int64(set.PluginMaxMB) << 20
	names := map[int64]string{}
	err = eachLine(im, filePlugins, func(p *xPlugin) error {
		what := fmt.Sprintf("plugin %d", p.Ref)
		if len(names) == MaxRoomPlugins {
			return refuseImport("more than %d plugins", MaxRoomPlugins)
		}
		if _, dup := names[p.Ref]; dup {
			return refuseImport("%s twice", what)
		}
		check := pluginpkg.CheckURL
		if p.Private {
			check = pluginpkg.CheckHostURL
		}
		if err := pluginpkg.CheckName(p.Name); err != nil {
			return refuseImport("%s: %v", what, err)
		}
		if err := check(p.URL); err != nil {
			return refuseImport("%s: its link: %v", what, err)
		}
		if err := pluginpkg.CheckRef(p.GitRef); err != nil {
			return refuseImport("%s: %v", what, err)
		}
		if p.Entry != "" {
			if err := pluginpkg.CheckName(p.Entry); err != nil {
				return refuseImport("%s: its entry: %v", what, err)
			}
		}
		if err := textIs(what, maxImportName, false, false, p.RefName, p.CreatedBy); err != nil {
			return err
		}
		var id int64
		err := im.tx.QueryRowContext(im.ctx, `INSERT INTO room_plugins(room_id, name, source_url, ref, ref_name, entry, private, created_by_name, created_at)
			VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, im.room, p.Name, p.URL, p.GitRef, p.RefName, p.Entry, p.Private, p.CreatedBy, p.CreatedAt).Scan(&id)
		if err != nil {
			return refuseImport("%s: %v", what, err)
		}
		names[p.Ref], im.plugins[p.Ref] = p.Name, id
		im.rep.Counts["plugins"]++
		return nil
	})
	if err != nil {
		return err
	}
	seen := map[int64][]int{}
	err = eachLine(im, filePluginVersions, func(v *xPluginVersion) error {
		id, ok := im.plugins[v.Plugin]
		if !ok {
			return refuseImport("version of unknown plugin %d", v.Plugin)
		}
		what := fmt.Sprintf("plugin %s v%d", names[v.Plugin], v.N)
		if v.N < 1 || slices.Contains(seen[v.Plugin], v.N) {
			return refuseImport("%s: a version twice, or numbered below 1", what)
		}
		seen[v.Plugin] = append(seen[v.Plugin], v.N)
		if !pluginCommitRe.MatchString(v.Commit) {
			return refuseImport("%s: %q is not a commit", what, v.Commit)
		}
		if err := textIs(what, maxImportName, false, false, v.AddedBy); err != nil {
			return err
		}
		// A refused or invalid version never loads: its record travels, its files stay behind.
		if v.File == "" || v.State == PluginRefused || v.State == PluginInvalid {
			return im.supersededVersion(id, what, v)
		}
		data, err := im.blob(v.File, what, pluginpkg.MaxTarBytes(maxBytes))
		if err != nil {
			return err
		}
		sn, err := pluginpkg.ValidateTar(bytes.NewReader(data), maxBytes)
		if err != nil {
			return refuseImport("%s: %v", what, err)
		}
		if sn.SHA256 != v.SHA256 {
			return refuseImport("%s: its files do not match its sha256", what)
		}
		m, err := sn.Manifest()
		if err != nil {
			return refuseImport("%s: %v", what, err)
		}
		files, _ := json.Marshal(sn.Files) // plain structs: cannot fail
		manifest, _ := json.Marshal(m)     // idem
		var vid int64
		err = im.tx.QueryRowContext(im.ctx, `INSERT INTO plugin_versions(plugin_id, n, commit_sha, sha256, size, files, file_list, manifest, state,
			added_by_name, added_at, hold) VALUES(?,?,?,?,?,?,?,?,?,?,?,1) RETURNING id`,
			id, v.N, v.Commit, sn.SHA256, sn.Size, len(sn.Files), string(files), string(manifest), PluginInspecting, v.AddedBy, v.AddedAt).Scan(&vid)
		if err != nil {
			return refuseImport("%s: %v", what, err)
		}
		if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO plugin_snapshots(version_id, data) VALUES(?,?)`, vid, sn.Tar); err != nil {
			return fmt.Errorf("import plugin snapshot: %w", err)
		}
		im.rep.Counts["plugin_versions"]++
		im.rep.Bytes["plugins"] += int64(len(sn.Tar))
		return nil
	})
	if err != nil {
		return err
	}
	for ref, name := range names {
		if len(seen[ref]) == 0 {
			return refuseImport("plugin %s has no version", name)
		}
	}
	for _, p := range ps {
		for _, ref := range p.Plugins {
			id, ok := im.plugins[ref]
			if !ok {
				return refuseImport("persona %s has plugin %d on, which the archive does not hold", p.Name, ref)
			}
			if err := activatePlugin(im.ctx, im.tx, im.personas[p.Ref], id, names[ref]); err != nil {
				return refuseImport("persona %s: plugin %s: %v", p.Name, names[ref], err)
			}
		}
	}
	return nil
}

// supersededVersion records version v of plugin id with no snapshot: one the export left out, or
// one refused or invalid, which keeps its state and reason. It never loads: a version the archive
// calls approved, pending or inspecting is recorded refused, so that it neither runs nor waits for
// a decision.
func (im *importer) supersededVersion(id int64, what string, v *xPluginVersion) error {
	if !json.Valid(v.FileList) || !json.Valid(v.Manifest) || (len(v.Report) > 0 && !json.Valid(v.Report)) {
		return refuseImport("%s: its file list, manifest or report is not JSON", what)
	}
	if !sha256Re.MatchString(v.SHA256) || v.Size < 0 || v.Files < 0 {
		return refuseImport("%s: bad sha256, size or file count", what)
	}
	if err := textIs(what, maxImportLineBody, false, true, v.Reason); err != nil {
		return err
	}
	if err := textIs(what, maxImportName, false, true, v.DecidedBy); err != nil {
		return err
	}
	state, reason := v.State, v.Reason
	switch state {
	case PluginRefused, PluginInvalid:
	case PluginApproved, PluginPending, PluginInspecting:
		state, reason = PluginRefused, supersededReason
	default:
		return refuseImport("%s: unknown state %q", what, v.State)
	}
	var report any
	if len(v.Report) > 0 {
		report = string(v.Report)
	}
	if _, err := im.tx.ExecContext(im.ctx, `INSERT INTO plugin_versions(plugin_id, n, commit_sha, sha256, size, files, file_list, manifest, report, state,
		reason, added_by_name, added_at, decided_by_name, decided_at, hold) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
		id, v.N, v.Commit, v.SHA256, v.Size, v.Files, string(v.FileList), string(v.Manifest), report, state, reason, v.AddedBy, v.AddedAt,
		nullStr(v.DecidedBy), nullStr(v.DecidedAt)); err != nil {
		return refuseImport("%s: %v", what, err)
	}
	im.rep.Counts["plugin_versions"]++
	return nil
}
