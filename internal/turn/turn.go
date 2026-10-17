// Package turn is the contract between the room server and a host daemon for one agent turn: the
// workspace layout, the job the daemon runs, and the result it posts back. docs/agent-workspace.md
// describes it for humans; this package has no dependency so the daemon binary stays small.
package turn

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Workspace layout, relative to the sandbox's /workspace.
const (
	// DocsDir holds the agent's documents, the only files whose changes become revisions.
	DocsDir = "docs"
	// UploadsDir holds participants' uploads, read-only (D30), plus the Markdown copy of office files.
	UploadsDir = "uploads"
	// TranscriptDir holds the room's messages as Markdown: a folder per UTC day, of chunks of
	// TranscriptChunk messages each (D20, 003-AC23).
	TranscriptDir = "transcript"
	// AnnotationsFile lists every annotation thread with its id.
	AnnotationsFile = "annotations.md"
	// AttachmentsDir holds the images attached to annotation messages.
	AttachmentsDir = "attachments"
	// RepliesFile is where the agent writes its annotation replies.
	RepliesFile = "replies.json"
	// NewAnnotationsFile is where the agent opens annotations of its own.
	NewAnnotationsFile = "annotations-new.json"
	// SpokenFile is what the agent says in the audio call after its turn (013-AC2).
	SpokenFile = "spoken.md"
	// MaxSpokenBytes caps it: 60 words fit many times over.
	MaxSpokenBytes = 4096
	// PluginName names the plugin that carries a persona's active skills: claude -p shows them as
	// "grill-room:<skill>".
	PluginName = "grill-room"
)

// Limits on what a result may carry.
const (
	MaxDocBytes  = 2 << 20
	MaxReplyLen  = 4000
	MaxDocName   = 200
	MaxSynthesis = 20000
	MaxModel     = 100
	MaxModelNote = 200
	// MaxNewAnnotations bounds the annotations one turn opens; the rest are ignored and reported.
	MaxNewAnnotations = 50
	// Anchor context, in runes, as the anchor package accepts it.
	maxContext = 64
	// TranscriptChunk is how many messages a transcript chunk holds. Counted in messages, not
	// lines: an edit that adds lines to a message must not move the later ones to another chunk.
	TranscriptChunk = 200
	// MinAutocompact and MaxAutocompact bound the token count claude -p --autocompact accepts.
	MinAutocompact = 100_000
	MaxAutocompact = 1_000_000
)

// HandoffDoc is the document a persona's last turn before a handoff writes for its new session,
// in docs/ (003-AC25).
func HandoffDoc(persona string) string { return "_handoff-" + persona + ".md" }

// Efforts are the values claude -p --effort accepts; a persona always names one (005-AC9).
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Models are the model aliases a persona may ask for; a persona always names one (005-AC9).
var Models = []string{"haiku", "sonnet", "opus", "fable"}

// Job is the turn the daemon runs, as the server hands it over.
type Job struct {
	ID     int64  `json:"id"`
	Prompt string `json:"prompt"`
	// Persona names the room persona the turn runs; System is its text, which claude gets as an
	// appended system prompt. Model and Effort are what it asks for; empty only from a room server
	// older than 005-AC9, which leaves them to Claude Code.
	Persona string `json:"persona,omitempty"`
	System  string `json:"system,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	// Approved is what the host approved for this persona above its cap, on this daemon only: the
	// daemon clamps to the higher of its cap and Approved (002-AC20).
	Approved Level `json:"approved"`
	// Resume is the session to continue; empty starts a new one.
	Resume string `json:"resume,omitempty"`
	// SessionSize and SessionSHA256 describe the server's copy of Resume: a daemon whose local copy
	// differs fetches the server's before resuming.
	SessionSize    int64  `json:"session_size"`
	SessionSHA256  string `json:"session_sha256,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	// Skills are the persona's active skills at the versions the room pinned (002-AC31). When set,
	// the daemon fetches them as a plugin directory and runs claude with --plugin-dir.
	Skills []Skill `json:"skills,omitempty"`
	// Plugins are the room plugins the persona has on, at the versions approved when the turn
	// started (009-AC17). The daemon fetches each tar by SHA256 and passes one --plugin-dir per
	// plugin after grill-room's.
	Plugins []Plugin `json:"plugins,omitempty"`
	// Autocompact is the persona's context size for claude -p --autocompact; 0 for auto (003-AC24).
	Autocompact int `json:"autocompact,omitempty"`
	// Cause is why the turn runs, as its preamble says; the daemon's monitoring records it (003-AC27).
	Cause string `json:"cause,omitempty"`
}

// Skill names a skill a turn loads, at its pinned version.
type Skill struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// Plugin names a room plugin a turn loads: its snapshot's sha256 and its version number.
type Plugin struct {
	// ID is the room's plugin id: the daemon keeps the option values under it (009-AC22).
	ID      int64  `json:"id,omitempty"`
	Name    string `json:"name"`
	SHA256  string `json:"sha256"`
	Version int    `json:"version"`
	// Options are the userConfig options the version declares, never a value: the daemon leaves
	// out a plugin whose required option the host has not set (009-AC24).
	Options []PluginOption `json:"options,omitempty"`
}

// PluginOption is one userConfig option of a plugin, as the daemon needs it at turn time.
type PluginOption struct {
	Key       string `json:"key"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Required  bool   `json:"required,omitempty"`
	Default   any    `json:"default,omitempty"`
}

// SkippedPlugin is a plugin the daemon left out of a turn, and why (009-AC24).
type SkippedPlugin struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// NeedsHostSetup is why a plugin with a required option unset does not load (009-AC24).
const NeedsHostSetup = "needs host setup"

// Change is one workspace file the turn created, modified or deleted. Content is sent only for the
// files NeedsContent names.
type Change struct {
	Path    string `json:"path"`
	Deleted bool   `json:"deleted,omitempty"`
	Content []byte `json:"content,omitempty"`
}

// Result is what the daemon posts at the end of a turn. A non-empty Error fails the turn.
type Result struct {
	Error     string `json:"error,omitempty"`
	Synthesis string `json:"synthesis,omitempty"`
	// Model is the model the turn asked for after the host cap, checked against what claude reported
	// (005-AC10). ModelNote names the other models claude reported with their cost share, "+opus 3%";
	// with ModelMismatch, claude did not report Model at all and the note lists what it did report.
	// Effort is the effort it ran with.
	Model         string `json:"model,omitempty"`
	ModelNote     string `json:"model_note,omitempty"`
	ModelMismatch bool   `json:"model_mismatch,omitempty"`
	Effort        string `json:"effort,omitempty"`
	// Capped reports that the host cap lowered the model or effort the persona asked for (002-AC19).
	Capped bool `json:"capped,omitempty"`
	// Session carries the session jsonl bytes from SessionOffset on (D23).
	SessionID     string   `json:"session_id,omitempty"`
	SessionOffset int64    `json:"session_offset"`
	Session       []byte   `json:"session,omitempty"`
	Changes       []Change `json:"changes,omitempty"`
	// ContextTokens is what the session's last request sent to the model: its input, cache read
	// and cache creation tokens. ContextWindow is the model's window as claude reported it. 0 is
	// unknown (003-AC24).
	ContextTokens int64 `json:"context_tokens,omitempty"`
	ContextWindow int64 `json:"context_window,omitempty"`
	// SkippedPlugins are the job's plugins the daemon left out (009-AC24).
	SkippedPlugins []SkippedPlugin `json:"skipped_plugins,omitempty"`
	// Blocked are the domains the turn's sandbox was refused a CONNECT to port 443 of, once each
	// (014-AC8).
	Blocked []string `json:"blocked,omitempty"`
}

// Manifest lists a turn's workspace (002-AC37): the daemon fetches only the files its cache lacks.
// Files are sorted by path.
type Manifest struct {
	Dirs  []string `json:"dirs"`
	Files []File   `json:"files"`
}

// File is one workspace file; SHA256 is the hex digest of its bytes.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   int64  `json:"mode"`
}

// PathsRequest asks for a tar of these workspace files only.
type PathsRequest struct {
	Paths []string `json:"paths"`
}

// FallbackReasons are why a daemon with a cache fetched the full workspace (002-AC38): it holds
// no cache for the room, a cached file failed its hash, the partial tar did not match the
// manifest, or the manifest was unavailable or invalid.
var FallbackReasons = []string{"missing", "corrupt", "partial", "manifest"}

// ValidPath accepts a relative, slash-separated workspace path with no "." or ".." element: it
// cannot leave the workspace.
func ValidPath(p string) bool { return p != "." && fs.ValidPath(p) }

// NeedsContent reports whether the server needs a changed file's bytes; for any other change the
// path is enough, which keeps an edited 20 MB upload out of the result.
func NeedsContent(p string) bool {
	return p == RepliesFile || p == NewAnnotationsFile || p == SpokenFile || strings.HasPrefix(p, DocsDir+"/")
}

// Doc is an agent document as a turn wrote it.
type Doc struct {
	Name   string
	Format string
	Body   string
}

// Reply is the agent's message on an annotation thread; Status "done" also closes the thread.
type Reply struct {
	Annotation int64  `json:"annotation"`
	Body       string `json:"body"`
	Status     string `json:"status,omitempty"`
}

// NewAnnotation is an annotation the agent opens: a text quote in the document at workspace path
// Doc, uploads included (annotating is not editing, D30), with the first message of its thread.
type NewAnnotation struct {
	Doc    string `json:"doc"`
	Exact  string `json:"exact"`
	Prefix string `json:"prefix"`
	Suffix string `json:"suffix"`
	Body   string `json:"body"`
}

// Outcome is a result's changes, sorted out.
type Outcome struct {
	Docs        []Doc
	Replies     []Reply
	Annotations []NewAnnotation
	// Spoken is spoken.md as the agent wrote it, unsanitised; empty when it wrote none.
	Spoken string
	// Rejected lists changed upload paths; Ignored, changes outside the contract with why.
	Rejected []string
	Ignored  []string
}

// Classify sorts a result's changes: documents, replies, refused upload changes, and ignored
// changes. Files elsewhere in the workspace are the agent's scratch and pass silently.
func Classify(changes []Change) Outcome {
	var o Outcome
	for _, c := range changes {
		p := path.Clean(c.Path)
		switch {
		case p == UploadsDir || strings.HasPrefix(p, UploadsDir+"/"):
			o.Rejected = append(o.Rejected, p)
		case p == RepliesFile && !c.Deleted:
			replies, err := parseReplies(c.Content)
			if err != nil {
				o.Ignored = append(o.Ignored, fmt.Sprintf("%s: %v", p, err))
			}
			o.Replies = append(o.Replies, replies...)
		case p == NewAnnotationsFile && !c.Deleted:
			anns, err := parseNewAnnotations(c.Content)
			if err != nil {
				o.Ignored = append(o.Ignored, fmt.Sprintf("%s: %v", p, err))
			}
			o.Annotations = append(o.Annotations, anns...)
		case p == SpokenFile && !c.Deleted:
			if len(c.Content) > MaxSpokenBytes || !utf8.Valid(c.Content) {
				o.Ignored = append(o.Ignored, fmt.Sprintf("%s: over %d bytes or not UTF-8", p, MaxSpokenBytes))
				continue
			}
			o.Spoken = string(c.Content)
		case strings.HasPrefix(p, DocsDir+"/"):
			d, err := doc(p, c)
			if err != nil {
				o.Ignored = append(o.Ignored, fmt.Sprintf("%s: %v", p, err))
				continue
			}
			o.Docs = append(o.Docs, d)
		}
	}
	return o
}

func doc(p string, c Change) (Doc, error) {
	if c.Deleted {
		return Doc{}, fmt.Errorf("documents cannot be deleted")
	}
	name := strings.TrimPrefix(p, DocsDir+"/")
	format := DocFormat(name)
	switch {
	case !ValidDocName(name):
		return Doc{}, fmt.Errorf("want a .md or .html file directly in %s/, named in at most %d plain characters", DocsDir, MaxDocName)
	case len(c.Content) > MaxDocBytes:
		return Doc{}, fmt.Errorf("over %d bytes", MaxDocBytes)
	case !utf8.Valid(c.Content):
		return Doc{}, fmt.Errorf("not UTF-8 text")
	}
	return Doc{Name: name, Format: format, Body: string(c.Content)}, nil
}

// DocFormat is the document format of a file name: md, html, or "" for any other.
func DocFormat(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		return "md"
	case ".html", ".htm":
		return "html"
	}
	return ""
}

// ValidDocName accepts a file name for an agent document: a .md or .html name with no directory,
// no control or bidi character, and at most MaxDocName characters.
func ValidDocName(name string) bool {
	if DocFormat(name) == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) ||
		!utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxDocName || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return false
		}
	}
	return true
}

func parseReplies(b []byte) ([]Reply, error) {
	var in []Reply
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf(`want [{"annotation": <id>, "body": "<reply>"}]: %w`, err)
	}
	var out []Reply
	var skipped int
	for _, r := range in {
		r.Body = strings.TrimSpace(r.Body)
		if r.Annotation <= 0 || r.Body == "" || (r.Status != "" && r.Status != "done") {
			skipped++
			continue
		}
		r.Body = cut(r.Body, MaxReplyLen)
		out = append(out, r)
	}
	if skipped > 0 {
		return out, fmt.Errorf(`%d replies without an annotation id or a body, or with a status other than "done"`, skipped)
	}
	return out, nil
}

func fold(s string) string { return strings.Join(strings.Fields(s), " ") }

func cut(s string, n int) string {
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n])
	}
	return s
}

func parseNewAnnotations(b []byte) ([]NewAnnotation, error) {
	var in []NewAnnotation
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf(`want [{"doc": "<path>", "exact": "<quote>", "prefix": "...", "suffix": "...", "body": "<comment>"}]: %w`, err)
	}
	var out []NewAnnotation
	var skipped int
	for _, a := range in {
		a.Doc, a.Body = path.Clean(strings.TrimSpace(a.Doc)), strings.TrimSpace(a.Body)
		// Whitespace folded as the anchor text is: a quote copied across a line break still matches.
		a.Exact, a.Prefix, a.Suffix = fold(a.Exact), fold(a.Prefix), fold(a.Suffix)
		if a.Doc == "." || a.Exact == "" || a.Body == "" || !utf8.ValidString(a.Exact+a.Prefix+a.Suffix) {
			skipped++
			continue
		}
		// Context longer than an anchor holds is trimmed from its far end: the near words locate.
		if rs := []rune(a.Prefix); len(rs) > maxContext {
			a.Prefix = string(rs[len(rs)-maxContext:])
		}
		a.Suffix = cut(a.Suffix, maxContext)
		a.Body = cut(a.Body, MaxReplyLen)
		out = append(out, a)
	}
	var errs []string
	if skipped > 0 {
		errs = append(errs, fmt.Sprintf("%d annotations without a doc, a quote or a body", skipped))
	}
	if len(out) > MaxNewAnnotations {
		errs = append(errs, fmt.Sprintf("%d annotations past the first %d", len(out)-MaxNewAnnotations, MaxNewAnnotations))
		out = out[:MaxNewAnnotations]
	}
	if len(errs) > 0 {
		return out, errors.New(strings.Join(errs, ", "))
	}
	return out, nil
}
