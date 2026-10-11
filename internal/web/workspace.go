package web

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// maxPromptTranscript bounds the transcript lines quoted in a prompt; older ones stay in transcript/.
const maxPromptTranscript = 100_000

// docPaths maps each document to the workspace file holding its text: an agent document in
// docs/, an upload's original in uploads/, or an office upload's Markdown copy beside it.
func docPaths(docs []store.Document) map[int64]string {
	m := make(map[int64]string, len(docs))
	for _, d := range docs {
		m[d.ID] = textPath(d)
	}
	return m
}

func agentPath(d store.Document) string {
	if turn.ValidDocName(d.Name) {
		return turn.DocsDir + "/" + d.Name
	}
	return fmt.Sprintf("%s/doc-%d.%s", turn.DocsDir, d.ID, d.Format)
}

// uploadPath prefixes the id: two uploads may share a name.
func uploadPath(d store.Document) string {
	return fmt.Sprintf("%s/%d-%s", turn.UploadsDir, d.ID, strings.ReplaceAll(d.Name, "/", "_"))
}

func isOffice(d store.Document) bool {
	return d.Kind == store.DocUpload && d.Format == "md" && !strings.HasPrefix(d.ContentType, "text/markdown")
}

func textPath(d store.Document) string {
	switch {
	case d.Kind == store.DocAgent:
		return agentPath(d)
	case isOffice(d):
		return uploadPath(d) + ".md"
	}
	return uploadPath(d)
}

// promptInput is what a turn's prompt tells its persona.
type promptInput struct {
	// Preamble opens the prompt: the presence preamble (002-AC28).
	Preamble string
	Room     string
	// Persona and Self name the persona the turn runs: its own messages are not news to it.
	Persona    string
	Self       int64
	NewSession bool
	Msgs       []store.Message
	Changes    []store.MessageChange
	Anns       []store.Annotation
	// AnnCursor is the last annotation message the persona's previous turn saw.
	AnnCursor int64
	Paths     map[int64]string
}

// ownMessage reports a thread message the persona self wrote.
func ownMessage(m store.AnnotationMessage, self int64) bool {
	return m.By == store.ByAgent && m.PersonaID == self
}

// buildPrompt is the turn's prompt (docs/agent-workspace.md): the workspace contract, then what
// was said, changed and annotated since the persona's last successful turn. The persona's role is
// its system prompt, not part of this.
func buildPrompt(in promptInput) string {
	var b strings.Builder
	b.WriteString(in.Preamble)
	fmt.Fprintf(&b, "You are %s, an agent of the grill session %q. Participants talk in the room; their speech reaches you as transcript lines. Other agents of the room may write there too, in lines tagged (agent). Someone asked for your turn.\n\n", in.Persona, in.Room)
	b.WriteString(`Your workspace is the current directory:
- docs/: the documents the room's agents write, Markdown (.md) or HTML (.html). Edit them or add new ones; every change becomes a revision the room sees. Give a new document a name no upload has: your version of uploads/<id>-plan.md goes in docs/plan-revised.md, for example. A new document named like an upload is stored as <name>-agent.md.
- uploads/: documents participants uploaded. They are read-only: never edit, move or delete them; write your own documents from them. Office files have a Markdown copy beside them, ending in .md.
- transcript/: everything said in the room, one Markdown file per day.
- annotations.md: every annotation thread on the documents, with its id. Images attached to threads are in attachments/.
- To answer annotation threads, write replies.json: [{"annotation": <id>, "body": "<your reply>"}]. Add "status": "done" to a reply that settles its thread; a participant who replies later reopens it.
- To point the participants at a passage, any document included, uploads too, write annotations-new.json: [{"doc": "<its path here, like uploads/3-plan.md>", "exact": "<the passage, word for word as it reads, without Markdown marks>", "prefix": "<the few words before it>", "suffix": "<the few words after it>", "body": "<your question or remark>"}]. Use it for what needs an answer from someone, not to repeat your synthesis.

`)
	if in.NewSession {
		b.WriteString("This is a new session: you remember no earlier turn. Read transcript/ for the room's history.\n\n")
	}
	msgs, changes, anns := in.Msgs, in.Changes, in.Anns
	if len(msgs) == 0 && len(changes) == 0 && len(anns) == 0 {
		b.WriteString("Nothing was said or annotated since your last turn.\n\n")
	}
	if len(changes) > 0 {
		b.WriteString("## Changed since your last turn\n\nLines you saw that their author edited or deleted since.\n\n")
		for _, c := range changes {
			b.WriteString(changeLine(c))
		}
		b.WriteString("\n")
	}
	if len(msgs) > 0 {
		b.WriteString("## Said since your last turn\n\n")
		lines := make([]string, len(msgs))
		size := 0
		first := len(msgs)
		for i := len(msgs) - 1; i >= 0; i-- {
			lines[i] = transcriptLine(msgs[i], true)
			if size += len(lines[i]); size > maxPromptTranscript {
				break
			}
			first = i
		}
		if first > 0 {
			fmt.Fprintf(&b, "(%d earlier lines are in transcript/.)\n", first)
		}
		for _, l := range lines[first:] {
			b.WriteString(l)
		}
		b.WriteString("\n")
	}
	if len(anns) > 0 {
		b.WriteString("## Annotation threads with new messages\n\n")
		for _, a := range anns {
			writeThread(&b, a, in.Paths, in.AnnCursor, in.Self)
		}
	}
	b.WriteString("Your final answer is posted in the room as your synthesis: say what you changed and what you need from the participants. Keep it short.\n")
	return b.String()
}

// transcriptLine renders one message; dated lines carry the day for the prompt, which spans days.
func transcriptLine(m store.Message, dated bool) string {
	layout := "15:04:05"
	if dated {
		layout = "2006-01-02 15:04:05"
	}
	ts := m.TS.UTC().Format(layout)
	body := indent(m.Body)
	if m.Kind == store.KindSystem {
		return fmt.Sprintf("- %s _%s_\n", ts, body)
	}
	var tags []string
	switch m.Kind {
	case store.KindVoice:
		tags = append(tags, "voice")
	case store.KindAgent:
		tags = append(tags, "agent")
	}
	switch {
	case m.DeletedAt != nil:
		tags = append(tags, "deleted")
	case m.EditedAt != nil:
		tags = append(tags, "edited")
	}
	who := "**" + m.Sender + "**"
	if len(tags) > 0 {
		who += " (" + strings.Join(tags, ", ") + ")"
	}
	return fmt.Sprintf("- %s %s: %s\n", ts, who, body)
}

func indent(body string) string { return strings.ReplaceAll(strings.TrimSpace(body), "\n", "\n  ") }

// changeLine tells the agent a line it saw changed: "edited: before → after", or "deleted".
func changeLine(c store.MessageChange) string {
	who := "**" + c.Sender + "**"
	if c.Kind == store.KindVoice {
		who += " (voice)"
	}
	ts := c.TS.UTC().Format("2006-01-02 15:04:05")
	if c.DeletedAt != nil {
		return fmt.Sprintf("- %s %s deleted: %s\n", ts, who, indent(c.Before))
	}
	return fmt.Sprintf("- %s %s edited: %s → %s\n", ts, who, indent(c.Before), indent(c.Body))
}

func describeAnchor(a *anchor.Anchor) string {
	switch {
	case a == nil:
		return "the whole document"
	case a.Kind == anchor.KindText:
		return fmt.Sprintf("the text %q", a.Exact)
	case a.Fingerprint != "":
		return fmt.Sprintf("a <%s> element reading %q", a.Tag, a.Fingerprint)
	}
	return fmt.Sprintf("a <%s> element", a.Tag)
}

// writeThread renders an annotation thread; messages above newAfter that persona self did not
// write are flagged new (0: none is).
func writeThread(b *strings.Builder, a store.Annotation, paths map[int64]string, newAfter, self int64) {
	fmt.Fprintf(b, "### Annotation %d on %s (%s)\n\nOn %s.", a.ID, paths[a.DocumentID], a.Status, describeAnchor(a.Anchor))
	if a.Orphaned {
		b.WriteString(" That text is gone from the latest revision.")
	}
	b.WriteString("\n\n")
	for _, m := range a.Messages {
		flag := ""
		if newAfter > 0 && m.ID > newAfter && !ownMessage(m, self) {
			flag = " (new)"
		}
		fmt.Fprintf(b, "- **%s**%s: %s\n", m.Author, flag, strings.ReplaceAll(strings.TrimSpace(m.Body), "\n", "\n  "))
		for _, at := range m.Attachments {
			fmt.Fprintf(b, "  - image: %s\n", attachmentPath(at))
		}
	}
	b.WriteString("\n")
}

func attachmentPath(at store.Attachment) string {
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[at.ContentType]
	return fmt.Sprintf("%s/%d%s", turn.AttachmentsDir, at.ID, ext)
}

// writeWorkspace writes the turn's workspace as a tar: the documents, the uploads (read-only) with
// their Markdown copies, the transcript up to the turn's cursor, and the annotation threads.
func (s *Server) writeWorkspace(ctx context.Context, w io.Writer, t *store.Turn) error {
	st := s.bus.Store
	tw := tar.NewWriter(w)
	mtime := time.Now()
	dir := func(name string) error {
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: mtime})
	}
	file := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: mtime}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	for _, d := range []string{turn.DocsDir, turn.UploadsDir, turn.TranscriptDir, turn.AttachmentsDir} {
		if err := dir(d); err != nil {
			return err
		}
	}
	docs, err := st.Documents(ctx, t.RoomID)
	if err != nil {
		return err
	}
	for _, d := range docs {
		if d.Kind == store.DocAgent {
			rev, err := st.Revision(ctx, d.ID, d.Revisions)
			if err != nil {
				return err
			}
			if err := file(agentPath(d), 0o644, []byte(rev.Body)); err != nil {
				return err
			}
			continue
		}
		// Read-only modes keep honest edits away; the server refuses any upload change anyway.
		data, err := st.Original(ctx, t.RoomID, d.ID)
		if err != nil {
			return err
		}
		if err := file(uploadPath(d), 0o444, data); err != nil {
			return err
		}
		if isOffice(d) {
			rev, err := st.Revision(ctx, d.ID, 1)
			if err != nil {
				return err
			}
			if err := file(textPath(d), 0o444, []byte(rev.Body)); err != nil {
				return err
			}
		}
	}
	if err := s.writeTranscript(ctx, t, file); err != nil {
		return err
	}
	anns, _, err := st.AnnotationsSince(ctx, t.RoomID, 0)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Annotation threads\n\n")
	if len(anns) == 0 {
		b.WriteString("None yet.\n")
	}
	paths := docPaths(docs)
	for _, a := range anns {
		writeThread(&b, a, paths, 0, 0)
		for _, m := range a.Messages {
			for _, at := range m.Attachments {
				if err := file(attachmentPath(at), 0o444, at.Data); err != nil {
					return err
				}
			}
		}
	}
	if err := file(turn.AnnotationsFile, 0o444, []byte(b.String())); err != nil {
		return err
	}
	return tw.Close()
}

// writeTranscript writes the room's messages up to the turn's cursor, one file per UTC day.
func (s *Server) writeTranscript(ctx context.Context, t *store.Turn, file func(string, int64, []byte) error) error {
	room, err := s.bus.Store.Room(ctx, t.RoomID)
	if err != nil {
		return err
	}
	var day string
	var buf bytes.Buffer
	flush := func() error {
		if day == "" {
			return nil
		}
		return file(path.Join(turn.TranscriptDir, day+".md"), 0o444, buf.Bytes())
	}
	for after := int64(0); after < t.MessageCursor; {
		batch, err := s.bus.Store.Messages(ctx, t.RoomID, after, replayBatch)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, m := range batch {
			if m.ID > t.MessageCursor {
				break
			}
			if d := m.TS.UTC().Format(time.DateOnly); d != day {
				if err := flush(); err != nil {
					return err
				}
				day = d
				buf.Reset()
				fmt.Fprintf(&buf, "# %s, %s (UTC)\n\n", room.Name, day)
			}
			buf.WriteString(transcriptLine(m, false))
		}
		after = batch[len(batch)-1].ID
	}
	return flush()
}
