package web

// The room's side of personas' voices (spec 013): the call settings room admins edit, the voice
// picker's catalogue and samples, and the samples the tts pod hands over.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/voices"
)

// maxSampleBytes bounds a sample the pod hands over: a few seconds of Opus.
const maxSampleBytes = 256 << 10

var sampleLangs = map[string]bool{"fr": true, "en": true}

// setSpeech changes the room's call settings, for room admins (013-AC25).
func (s *Server) setSpeech(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.asRoomAdmin(w, r, sess) {
		return
	}
	room := r.PathValue("room")
	cur, err := s.bus.Store.Room(r.Context(), room)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	if err != nil {
		s.serverError(w, "set_speech_failed", err)
		return
	}
	var req struct {
		QuietMS *int `json:"quiet_ms"`
		ResumeS *int `json:"resume_s"`
		QueueS  *int `json:"queue_s"`
		Words   *int `json:"words"`
	}
	if !decode(w, r, &req) {
		return
	}
	sp := cur.Speech
	for _, f := range []struct {
		v   *int
		dst *int
	}{{req.QuietMS, &sp.QuietMS}, {req.ResumeS, &sp.ResumeS}, {req.QueueS, &sp.QueueS}, {req.Words, &sp.Words}} {
		if f.v != nil {
			*f.dst = *f.v
		}
	}
	if !sp.Valid() {
		lo, hi := store.SpeechMin, store.SpeechMax
		httpError(w, http.StatusBadRequest, fmt.Sprintf("quiet_ms %d to %d, resume_s %d to %d, queue_s %d to %d, words %d to %d",
			lo.QuietMS, hi.QuietMS, lo.ResumeS, hi.ResumeS, lo.QueueS, hi.QueueS, lo.Words, hi.Words))
		return
	}
	var by *store.Participant
	if sess != nil {
		by = sess.p
	}
	m, err := s.bus.Store.SetSpeech(s.roomAuditing(r, sess, ActionRoomSpeech, room, nil), room, sp, by)
	if err != nil {
		s.serverError(w, "set_speech_failed", err)
		return
	}
	if m != nil {
		s.bus.Notify(room)
	}
	cur.Speech = sp
	writeJSON(w, http.StatusOK, cur)
}

// voiceChoice is a catalogue voice as the picker shows it (013-AC27).
type voiceChoice struct {
	voices.Voice
	// Samples are the languages whose sample the room server holds.
	Samples []string `json:"samples"`
}

// listVoices answers the voice picker: the catalogue with its samples, the room's language and
// which persona holds which voice. ?lang= is the browser's language, for a room with no line yet:
// speaking personas with no voice get their default from it now (013-AC16).
func (s *Server) listVoices(w http.ResponseWriter, r *http.Request, sess session) {
	ctx := r.Context()
	room := sess.p.RoomID
	fallback := "en"
	if r.URL.Query().Get("lang") == "fr" {
		fallback = "fr"
	}
	ps, err := s.bus.Store.RoomPersonas(ctx, room)
	if err != nil {
		s.serverError(w, "list_voices_failed", err)
		return
	}
	s.withVoices(ctx, room, ps, fallback)
	hashes, err := s.bus.Store.SampleHashes(ctx)
	if err != nil {
		s.serverError(w, "list_voices_failed", err)
		return
	}
	out := []voiceChoice{}
	for _, v := range voices.All() {
		c := voiceChoice{Voice: v, Samples: []string{}}
		for _, lang := range []string{"fr", "en"} {
			if hashes[v.ID+"/"+lang] != "" {
				c.Samples = append(c.Samples, lang)
			}
		}
		out = append(out, c)
	}
	used := map[string]int64{}
	for _, p := range ps {
		if p.Voice != "" {
			used[p.Voice] = p.ID
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"voices": out, "lang": s.roomLang(ctx, room, fallback), "used": used})
}

// voiceSample serves a voice's sample, rendered at the image's build: it plays while the pod is
// down (013-AC27).
func (s *Server) voiceSample(w http.ResponseWriter, r *http.Request, _ session) {
	x, err := s.bus.Store.GetSample(r.Context(), r.PathValue("voice"), r.PathValue("lang"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no sample for this voice")
		return
	}
	if err != nil {
		s.serverError(w, "voice_sample_failed", err)
		return
	}
	w.Header().Set("Content-Type", "audio/ogg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("ETag", `"`+x.SHA256+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(x.Data))
}

// ttsSampleHashes tells the pod which samples the server holds, so it hands over only the others.
func (s *Server) ttsSampleHashes(w http.ResponseWriter, r *http.Request) {
	h, err := s.bus.Store.SampleHashes(r.Context())
	if err != nil {
		s.serverError(w, "sample_hashes_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": h})
}

// ttsPutSample keeps a sample the pod hands over, for a catalogue voice only (013-AC11).
func (s *Server) ttsPutSample(w http.ResponseWriter, r *http.Request) {
	voice, lang := r.PathValue("voice"), r.PathValue("lang")
	if _, ok := voices.Get(voice); !ok || !sampleLangs[lang] {
		httpError(w, http.StatusBadRequest, "not a catalogue voice, or lang not fr or en")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSampleBytes))
	if err != nil {
		httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a sample holds at most %d bytes", maxSampleBytes))
		return
	}
	if len(data) == 0 {
		httpError(w, http.StatusBadRequest, "empty sample")
		return
	}
	sum := sha256.Sum256(data)
	if err := s.bus.Store.PutSample(r.Context(), store.Sample{Voice: voice, Lang: lang, SHA256: hex.EncodeToString(sum[:]), Data: data}); err != nil {
		s.serverError(w, "put_sample_failed", err)
		return
	}
	s.log.Info("tts_sample_kept", zap.String("voice", voice), zap.String("lang", lang), zap.Int("bytes", len(data)))
	w.WriteHeader(http.StatusNoContent)
}
