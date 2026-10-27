package web

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/archive"
	"github.com/babs/grill-room/internal/store"
)

// Audit actions of room exports and imports (004-AC6, 004-AC11, 004-AC12).
const (
	ActionRoomExport = "room.export"
	ActionRoomImport = "room.import"
)

const (
	// DefaultMaxImportBytes bounds an archive once uncompressed; maxImportFiles, its files. Both
	// cap a decompression bomb before it reaches the disk (004-AC14).
	DefaultMaxImportBytes = 1 << 30
	maxImportFiles        = 50_000
	// archiveIOTimeout bounds how long an import's upload or an export's download may take: a slow
	// client would otherwise hold one of the archive slots for good.
	archiveIOTimeout = 30 * time.Minute
	// stagingPrefix names the directories exports and imports stage files in, under StagingDir.
	stagingPrefix = ".archive-"
)

// stagingDir creates a directory to stage an export or import in, under opts.StagingDir.
func (s *Server) stagingDir() (string, error) {
	base := s.opts.StagingDir
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("staging dir: %w", err)
	}
	dir, err := os.MkdirTemp(base, stagingPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("staging dir: %w", err)
	}
	return dir, nil
}

// PrepareStaging creates dir, where exports and imports stage whole rooms, and makes sure only
// this process's user reaches it: it refuses a symlink, a non-directory or another user's
// directory, and narrows one of ours to 0700. Owner and mode go unchecked off Linux and macOS.
func PrepareStaging(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("staging dir %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("staging dir %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("staging dir %s: not a directory (%s)", dir, fi.Mode().Type())
	}
	uid, known := fileOwner(fi)
	if !known {
		return nil
	}
	if uid != os.Getuid() {
		return fmt.Errorf("staging dir %s: owned by uid %d, not by this process's %d", dir, uid, os.Getuid())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
			return fmt.Errorf("staging dir %s: %w", dir, err)
		}
	}
	return nil
}

// cleanStaging checks the staging directory and removes what exports and imports left staged
// there when the server stopped mid-way.
func (s *Server) cleanStaging() {
	if s.opts.StagingDir == "" {
		return
	}
	if err := PrepareStaging(s.opts.StagingDir); err != nil {
		s.log.Warn("staging_dir_failed", zap.String("dir", s.opts.StagingDir), zap.Error(err))
		return
	}
	left, _ := filepath.Glob(filepath.Join(s.opts.StagingDir, stagingPrefix+"*"))
	for _, d := range left {
		if err := os.RemoveAll(d); err != nil {
			s.log.Warn("staging_cleanup_failed", zap.String("dir", d), zap.Error(err))
		}
	}
}

// archiveSlot takes one of the archive slots, or answers 503: each export or import stages a whole
// room on disk and holds a database connection while it reads or writes it, an import the write
// lock too.
func (s *Server) archiveSlot(w http.ResponseWriter) (func(), bool) {
	select {
	case s.archives <- struct{}{}:
		return func() { <-s.archives }, true
	default:
		httpError(w, http.StatusServiceUnavailable, "another export or import is running, retry in a moment")
		return nil, false
	}
}

// exportRoom streams the room as a tar.gz, for its room admins and the super-admin: its files are
// staged on disk from one read of the store, then packed and hashed as they go out (004-AC11).
func (s *Server) exportRoom(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.asRoomAdmin(w, r, sess) {
		return
	}
	release, ok := s.archiveSlot(w)
	if !ok {
		return
	}
	defer release()
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(archiveIOTimeout)); err != nil {
		s.serverError(w, "export_failed", err)
		return
	}
	dir, err := s.stagingDir()
	if err != nil {
		s.serverError(w, "export_failed", err)
		return
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best effort: cleanStaging catches leftovers at the next start
	id := r.PathValue("room")
	room, err := s.bus.Store.ExportRoom(s.roomAuditing(r, sess, ActionRoomExport, id, nil), id, dir)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	if err != nil {
		s.serverError(w, "export_failed", err)
		return
	}
	inst, err := s.bus.Store.InstanceID(r.Context())
	if err != nil {
		s.serverError(w, "export_failed", err)
		return
	}
	at := s.now().UTC()
	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": "grill-room-" + room.ID + "-" + at.Format("20060102-150405") + ".tar.gz"}))
	m := archive.Manifest{InstanceVersion: s.opts.Version, InstanceID: inst, CreatedAt: at, Room: archive.Room{ID: room.ID, Name: room.Name}}
	if err := archive.Pack(w, dir, m); err != nil {
		// The answer has started: the client sees a cut archive, which fails its manifest check.
		s.log.Error("export_stream_failed", zap.String("room", id), zap.Error(err))
		return
	}
	s.log.Info("room_exported", zap.String("room", id), zap.Bool("by_room_admin", sess != nil))
}

// importResp is an import's report with the archive's manifest facts (004-AC15).
type importResp struct {
	*store.ImportReport
	Format          int          `json:"format"`
	InstanceVersion string       `json:"instance_version"`
	SameInstance    bool         `json:"same_instance"`
	ExportedAt      time.Time    `json:"exported_at"`
	Source          archive.Room `json:"source"`
	ArchiveBytes    int64        `json:"archive_bytes"`
}

// importRoom creates a room from an export, for the super-admin or a profile allowed to create
// rooms; with ?preview=1 it runs the whole import, checks included, and keeps nothing (004-AC12 to
// 004-AC15). The body is the tar.gz itself; a profile not among the room's participants names its
// seat with ?display_name=.
func (s *Server) importRoom(w http.ResponseWriter, r *http.Request) {
	ar, answered := s.asAdmin(w, r)
	if answered {
		return
	}
	var prof *store.Profile
	if ar == nil {
		if prof = s.profileFor(w, r); prof == nil {
			return
		}
		// An emailless profile is one anyone can make while anonymous rooms are on (019-AC9).
		if s.opts.AnonymousRooms && prof.Email == "" {
			httpError(w, http.StatusForbidden, "Add an email to your profile to import a room")
			return
		}
		if s.refuseExternal(w, prof) {
			return
		}
	} else {
		r = ar
	}
	// A cross-site form cannot send this type without a preflight.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/gzip" {
		httpError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/gzip")
		return
	}
	q := r.URL.Query()
	preview := q.Get("preview") == "1"
	display := ""
	if v := q.Get("display_name"); v != "" {
		var ok bool
		if display, ok = cleanText(v, maxNameLen, false); !ok {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("display name must be 1 to %d characters", maxNameLen))
			return
		}
	}
	release, ok := s.archiveSlot(w)
	if !ok {
		return
	}
	defer release()
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(archiveIOTimeout)); err != nil {
		s.serverError(w, "import_failed", err)
		return
	}
	dir, err := s.stagingDir()
	if err != nil {
		s.serverError(w, "import_failed", err)
		return
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best effort: cleanStaging catches leftovers at the next start
	m, err := archive.Unpack(http.MaxBytesReader(w, r.Body, s.opts.MaxImportBytes), dir, archive.Limits{MaxBytes: s.opts.MaxImportBytes, MaxFiles: maxImportFiles})
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the archive is over the %s this instance imports", sizeText(s.opts.MaxImportBytes)))
		return
	case errors.Is(err, archive.ErrRefused):
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.serverError(w, "import_failed", err)
		return
	}
	inst, err := s.bus.Store.InstanceID(r.Context())
	if err != nil {
		s.serverError(w, "import_failed", err)
		return
	}
	o := store.ImportOptions{RoomID: strings.ToLower(NewToken()), Format: m.Format, DisplayName: display, SameInstance: m.InstanceID == inst, SourceRoom: m.Room.ID, ArchiveBytes: m.Bytes(),
		DefaultQuota: s.opts.MaxRoomBytes, MaxUpload: s.opts.MaxUploadBytes, MaxAttachment: s.opts.MaxAttachmentBytes, CheckPersona: checkPersona, DryRun: preview}
	details := map[string]any{"from": m.Room.ID, "from_instance_version": m.InstanceVersion, "same_instance": o.SameInstance}
	var ctx context.Context
	if prof == nil {
		ctx = s.auditing(r, ActionRoomImport, store.TargetRoom, o.RoomID, "", details)
	} else {
		o.Importer, details["as"] = prof.ID, display
		ctx = store.WithAudit(r.Context(), store.AuditEntry{At: s.now(), Actor: ActorProfile + prof.UUID, Action: ActionRoomImport,
			TargetKind: store.TargetRoom, TargetID: o.RoomID}, details)
	}
	rep, err := s.bus.Store.ImportRoom(ctx, dir, o)
	switch {
	case errors.Is(err, store.ErrImportRefused):
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, store.ErrCreationRefused):
		httpError(w, http.StatusForbidden, err.Error())
		return
	case err != nil:
		s.serverError(w, "import_failed", err)
		return
	}
	resp := importResp{ImportReport: rep, Format: m.Format, InstanceVersion: m.InstanceVersion, SameInstance: o.SameInstance, ExportedAt: m.CreatedAt,
		Source: m.Room, ArchiveBytes: m.Bytes()}
	if !o.SameInstance {
		rep.Warnings = append(rep.Warnings, "Exported by another instance: its profiles and library are not this instance's.")
	}
	if m.InstanceVersion != s.opts.Version {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("Exported by version %s; this instance runs %s.", m.InstanceVersion, s.opts.Version))
	}
	if preview {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	s.log.Info("room_imported", zap.String("room", o.RoomID), zap.String("from", m.Room.ID), zap.Bool("same_instance", o.SameInstance), zap.Int64("profile", o.Importer))
	writeJSON(w, http.StatusCreated, resp)
}

// checkPersona validates an imported or starter persona as the room API validates one.
func checkPersona(p store.StarterPersona) error {
	_, err := cleanPersona(p)
	return err
}

// cleanPersona returns p as the room API would store it, or why it may not.
func cleanPersona(p store.StarterPersona) (store.StarterPersona, error) {
	q := personaReq{Name: &p.Name, Badge: &p.Badge, Colour: &p.Colour, Text: &p.Text, Model: &p.Model, Effort: &p.Effort, EveryN: &p.EveryN,
		EveryVoice: &p.EveryVoice, EveryMinutes: &p.EveryMinutes, Autocompact: &p.Autocompact, Voice: &p.Voice}
	if p.VoiceRate != 0 {
		q.VoiceRate = &p.VoiceRate
	}
	if why := q.check(); why != "" {
		return p, errors.New(why)
	}
	return p, nil
}
