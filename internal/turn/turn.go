// Package turn is the contract between the room server and a host daemon for one agent turn: the
// workspace layout, the job the daemon runs, and the result it posts back. docs/agent-workspace.md
// describes it for humans; this package has no dependency so the daemon binary stays small.
package turn

import (
	"encoding/json"
	"fmt"
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
	// TranscriptDir holds the room's messages as Markdown, one file per UTC day (D20).
	TranscriptDir = "transcript"
	// AnnotationsFile lists every annotation thread with its id.
	AnnotationsFile = "annotations.md"
	// AttachmentsDir holds the images attached to annotation messages.
	AttachmentsDir = "attachments"
	// RepliesFile is where the agent writes its annotation replies.
	RepliesFile = "replies.json"
)

// Limits on what a result may carry.
const (
	MaxDocBytes  = 2 << 20
	MaxReplyLen  = 4000
	MaxDocName   = 200
	MaxSynthesis = 20000
)

// Job is the turn the daemon runs, as the server hands it over.
type Job struct {
	ID     int64  `json:"id"`
	Prompt string `json:"prompt"`
	// Resume is the session to continue; empty starts a new one.
	Resume string `json:"resume,omitempty"`
	// SessionSize and SessionSHA256 describe the server's copy of Resume: a daemon whose local copy
	// differs fetches the server's before resuming.
	SessionSize    int64  `json:"session_size"`
	SessionSHA256  string `json:"session_sha256,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

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
	// Session carries the session jsonl bytes from SessionOffset on (D23).
	SessionID     string   `json:"session_id,omitempty"`
	SessionOffset int64    `json:"session_offset"`
	Session       []byte   `json:"session,omitempty"`
	Changes       []Change `json:"changes,omitempty"`
}

// NeedsContent reports whether the server needs a changed file's bytes; for any other change the
// path is enough, which keeps an edited 20 MB upload out of the result.
func NeedsContent(p string) bool {
	return p == RepliesFile || strings.HasPrefix(p, DocsDir+"/")
}

// Doc is an agent document as a turn wrote it.
type Doc struct {
	Name   string
	Format string
	Body   string
}

// Reply is the agent's message on an annotation thread.
type Reply struct {
	Annotation int64  `json:"annotation"`
	Body       string `json:"body"`
}

// Outcome is a result's changes, sorted out.
type Outcome struct {
	Docs    []Doc
	Replies []Reply
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
		if r.Annotation <= 0 || r.Body == "" {
			skipped++
			continue
		}
		if rs := []rune(r.Body); len(rs) > MaxReplyLen {
			r.Body = string(rs[:MaxReplyLen])
		}
		out = append(out, r)
	}
	if skipped > 0 {
		return out, fmt.Errorf("%d replies without an annotation id or a body", skipped)
	}
	return out, nil
}
