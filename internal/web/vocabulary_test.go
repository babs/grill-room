package web

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// phrase compiles a rule whose words may be split by any whitespace or &nbsp;.
func phrase(re string) *regexp.Regexp {
	return regexp.MustCompile("(?i)" + strings.ReplaceAll(re, " ", `(?:\s|&nbsp;)+`))
}

// One vocabulary (038-AC15): each concept has one name where a user reads it. A rule with a group
// flags a match only when the group is unmatched (the right form wraps the old one).
var vocabRules = []struct {
	re  *regexp.Regexp
	use string
}{
	{phrase(`\bdaemons?\b`), `"AI host" / "the AI host app"`},
	{phrase(`\bannotat`), `"note"`},
	{phrase(`comment on the document`), `"note"`},
	{phrase(`\b(?:join|leave) audio\b`), `"Join the call" / "Leave the call"`},
	{phrase(`mute the room|room (?:is )?muted|\bdeafen`), `"Stop hearing the call"`},
	{phrase(`(with )?everyone on this server`), `"Shared with everyone on this server"`},
	{phrase(`shared with everyone( on this server)?`), `"Shared with everyone on this server"`},
	{phrase(`\bholds? the agents?\b|\bits page\b`), `"the AI host app"`},
	{phrase(`\bapproves? everything\b|\bapprove-all\b`), `"Hold new AI personas and changes for my approval"`},
}

// Text that must keep an old word: in file, on a line holding line, only the words listed.
var vocabAllow = []struct {
	file, line string
	words      []string
	why        string
}{
	{"ui/room.js", "'~/Downloads/grill-room-daemon-'", []string{"daemon"}, "the download's file name, in the macOS command"},
	{"../../README.md", `"Bob holds the agent(s)."`, []string{"holds the agent"}, "quotes the server's chat line (internal/store)"},
	{"../../README.md", `"Alice holds the agent(s)"`, []string{"holds the agent"}, "quotes the server's chat line (internal/store)"},
	{"../../docs/design.md", "| host, holds the agent(s), Host the agent |", []string{"holds the agent"}, "the wording table names what it replaces"},
	{"../../docs/design.md", "| Deafen, Mute the room, Room muted |", []string{"Deafen", "Mute the room", "Room muted"}, "the wording table names what it replaces"},
	{"../../docs/design.md", "| Join audio, Leave audio |", []string{"Join audio", "Leave audio"}, "the wording table names what it replaces"},
	{"../../docs/design.md", "| approves everything, approve-all |", []string{"approves everything", "approve-all"}, "the wording table names what it replaces"},
	{"../../docs/design.md", "| annotation, Annotate, Comment on the document |", []string{"annotat", "Comment on the document"}, "the wording table names what it replaces"},
	{"../../docs/design.md", "| Everyone on this server, Shared with everyone |", []string{"Everyone on this server", "Shared with everyone"}, "the wording table names what it replaces"},
	{"../../docs/design.md", "| daemon, agent daemon, grill-room-daemon", []string{"daemon", "its page"}, "the wording table names what it replaces"},
}

// Vendored bundles, not our strings.
var vocabSkip = map[string]bool{"ui/vendor": true, "ui/stt/vendor": true, "ui/doc/mermaid.min.js": true, "ui/doc/tex-mml-chtml.js": true}

// What counts as visible text, kept simple:
//   - HTML: text nodes and the visibleAttr values (style and comments dropped); inline scripts as JS.
//   - JS: string and template literal text, minus code, comments, regexes, ${} expressions and `quoted`
//     commands; minus identifier-like strings (no space, lowercase/punctuation only: keys, events,
//     classes, file names), strings starting with a path or a selector (/x, .x, #x, [x), and markup
//     attributes other than the visible ones. The ceiling: a lone lowercase word reads as an identifier.
//   - Markdown: prose by paragraph, outside fences, code spans and comments, link targets and URLs dropped.
//   - Web app manifests: the name, short_name and description values.
var (
	attrValue   = "(\"[^\"]*\"|'[^']*'|[^\\s\"'<>=`]+)"
	visibleAttr = regexp.MustCompile(`\b(aria-label|aria-description|title|data-tip|placeholder|alt|value|label)\s*=\s*` + attrValue)
	anyAttr     = regexp.MustCompile(`([\w:.@-]+)\s*=\s*` + attrValue)
	textNode    = regexp.MustCompile(`>([^<>]+)`)
	script      = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script>`)
	hidden      = regexp.MustCompile(`(?is)<style\b.*?</style>|<!--.*?-->`)
	identLike   = regexp.MustCompile(`^[a-z0-9_.:/~#*@?&%+=\[\]-]*$`)
	pathOrSel   = regexp.MustCompile(`^(/|[.#\[][\w-])`)
	jsCode      = regexp.MustCompile("`[^`]*`")
	mdCode      = regexp.MustCompile("`[^`]*`|\\]\\([^)]*\\)|https?://\\S+")
	mdComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
	manifest    = regexp.MustCompile(`"(?:name|short_name|description)"\s*:\s*"([^"]*)"`)
	// A / after one of these starts a regex, not a division.
	regexAfterPunct = "(,=:[!&|?{};+-*%<>~^}"
	regexAfterWord  = map[string]bool{"return": true, "typeof": true, "case": true, "in": true, "of": true, "void": true,
		"delete": true, "throw": true, "yield": true, "await": true, "else": true}
)

type vspan struct {
	off  int
	text string
}

// blank keeps offsets and line numbers while dropping text from the scan.
func blank(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		return ' '
	}, s)
}

// unquote returns an attribute value without its quotes, and its offset in the value.
func unquote(v string) (string, int) {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		return v[1 : len(v)-1], 1
	}
	return v, 0
}

func htmlSpans(src string) []vspan {
	var out []vspan
	for _, m := range script.FindAllStringSubmatchIndex(src, -1) {
		for _, sp := range jsSpans(src[m[2]:m[3]]) {
			out = append(out, vspan{m[2] + sp.off, sp.text})
		}
	}
	src = hidden.ReplaceAllStringFunc(script.ReplaceAllStringFunc(src, blank), blank)
	for _, m := range textNode.FindAllStringSubmatchIndex(src, -1) {
		out = append(out, vspan{m[2], src[m[2]:m[3]]})
	}
	for _, m := range visibleAttr.FindAllStringSubmatchIndex(src, -1) {
		if v, q := unquote(src[m[4]:m[5]]); !identLike.MatchString(v) {
			out = append(out, vspan{m[4] + q, v})
		}
	}
	return out
}

type jsScan struct {
	src string
	out []vspan
}

func (s *jsScan) add(start, end int) {
	t := s.src[start:end]
	if strings.TrimSpace(t) == "" || pathOrSel.MatchString(t) || identLike.MatchString(t) {
		return
	}
	// A command quoted in backticks names a binary; markup keeps only text and visible attributes.
	t = jsCode.ReplaceAllStringFunc(t, blank)
	t = anyAttr.ReplaceAllStringFunc(t, func(a string) string {
		if m := visibleAttr.FindStringSubmatch(a); m != nil {
			if v, _ := unquote(m[2]); !identLike.MatchString(v) {
				return a
			}
		}
		return blank(a)
	})
	s.out = append(s.out, vspan{start, t})
}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// regexStart says whether a / at i opens a regex: after punctuation or a keyword, not after a value.
func (s *jsScan) regexStart(i int, prev byte) bool {
	if prev == 0 || strings.IndexByte(regexAfterPunct, prev) >= 0 {
		return true
	}
	j := i - 1
	for j >= 0 && (s.src[j] == ' ' || s.src[j] == '\t' || s.src[j] == '\n' || s.src[j] == '\r') {
		j--
	}
	k := j
	for k >= 0 && isIdent(s.src[k]) {
		k--
	}
	return regexAfterWord[s.src[k+1:j+1]]
}

// code scans JS from i; inExpr stops after the } closing a template's ${.
func (s *jsScan) code(i int, inExpr bool) int {
	depth, prev := 0, byte(0)
	for i < len(s.src) {
		c := s.src[i]
		next := byte(0)
		if i+1 < len(s.src) {
			next = s.src[i+1]
		}
		switch {
		case c == '/' && next == '/':
			if j := strings.IndexByte(s.src[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(s.src)
			}
			continue
		case c == '/' && next == '*':
			if j := strings.Index(s.src[i+2:], "*/"); j >= 0 {
				i += j + 4
			} else {
				i = len(s.src)
			}
			continue
		case c == '/' && s.regexStart(i, prev):
			i, prev = s.regex(i), 'a'
			continue
		case c == '\'' || c == '"':
			i, prev = s.quoted(i), 'a'
			continue
		case c == '`':
			i, prev = s.template(i), 'a'
			continue
		case c == '{':
			depth++
		case c == '}':
			if inExpr && depth == 0 {
				return i + 1
			}
			depth--
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			prev = c
		}
		i++
	}
	return i
}

func (s *jsScan) quoted(i int) int {
	j := i + 1
	for j < len(s.src) && s.src[j] != s.src[i] && s.src[j] != '\n' {
		if s.src[j] == '\\' {
			j++
		}
		j++
	}
	j = min(j, len(s.src))
	s.add(i+1, j)
	return j + 1
}

func (s *jsScan) template(i int) int {
	start := i + 1
	for j := start; j < len(s.src); j++ {
		switch {
		case s.src[j] == '\\':
			j++
		case s.src[j] == '`':
			s.add(start, j)
			return j + 1
		case s.src[j] == '$' && j+1 < len(s.src) && s.src[j+1] == '{':
			s.add(start, j)
			j = s.code(j+2, true)
			start = j
			j--
		}
	}
	return len(s.src)
}

func (s *jsScan) regex(i int) int {
	class := false
	for j := i + 1; j < len(s.src) && s.src[j] != '\n'; j++ {
		switch c := s.src[j]; {
		case c == '\\':
			j++
		case c == '[':
			class = true
		case c == ']':
			class = false
		case c == '/' && !class:
			return j + 1
		}
	}
	return i + 1
}

func jsSpans(src string) []vspan {
	s := &jsScan{src: src}
	s.code(0, false)
	return s.out
}

// mdSpans returns each paragraph (lines up to a blank line or a fence) as one span.
func mdSpans(src string) []vspan {
	src = mdComment.ReplaceAllStringFunc(src, blank)
	var out []vspan
	fence, off, start := false, 0, -1
	flush := func(end int) {
		if start >= 0 {
			out = append(out, vspan{start, mdCode.ReplaceAllStringFunc(src[start:end], blank)})
		}
		start = -1
	}
	for _, line := range strings.SplitAfter(src, "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "```"):
			flush(off)
			fence = !fence
		case fence:
		case strings.TrimSpace(line) == "":
			flush(off)
		case start < 0:
			start = off
		}
		off += len(line)
	}
	flush(off)
	return out
}

func manifestSpans(src string) []vspan {
	var out []vspan
	for _, m := range manifest.FindAllStringSubmatchIndex(src, -1) {
		out = append(out, vspan{m[2], src[m[2]:m[3]]})
	}
	return out
}

// vocabHits returns "line: word, use ..." for each replaced word in the visible text of src, and
// marks in used each allow-list word that let one through.
func vocabHits(name, src string, used map[string]bool) []string {
	var spans []vspan
	switch filepath.Ext(name) {
	case ".html":
		spans = htmlSpans(src)
	case ".js":
		spans = jsSpans(src)
	case ".md":
		spans = mdSpans(src)
	case ".webmanifest":
		spans = manifestSpans(src)
	}
	var hits []string
	for _, sp := range spans {
		for _, r := range vocabRules {
			for _, m := range r.re.FindAllStringSubmatchIndex(sp.text, -1) {
				if len(m) > 2 && m[2] >= 0 {
					continue
				}
				off := sp.off + m[0]
				ls := strings.LastIndexByte(src[:off], '\n') + 1
				le := strings.IndexByte(src[off:], '\n')
				if le < 0 {
					le = len(src) - off
				}
				word := strings.Join(strings.Fields(strings.ReplaceAll(sp.text[m[0]:m[1]], "&nbsp;", " ")), " ")
				if key := vocabAllowed(name, src[ls:off+le], word); key != "" {
					if used != nil {
						used[key] = true
					}
					continue
				}
				hits = append(hits, fmt.Sprintf("%d: %q, use %s", strings.Count(src[:off], "\n")+1, word, r.use))
			}
		}
	}
	return hits
}

// vocabAllowed returns the key of the allow-list word that lets word through on line, or "".
func vocabAllowed(name, line, word string) string {
	for i, a := range vocabAllow {
		if a.file != name || !strings.Contains(line, a.line) {
			continue
		}
		for _, w := range a.words {
			if strings.EqualFold(w, word) {
				return allowKey(i, w)
			}
		}
	}
	return ""
}

func allowKey(i int, word string) string { return fmt.Sprintf("%d/%s", i, word) }

func TestVocabularyInVisibleText(t *testing.T) {
	var files []string
	for _, root := range []string{"ui", "../daemon/ui"} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// Windows walks with backslashes; the skip and allow lists name files with slashes.
			p = filepath.ToSlash(p)
			if vocabSkip[p] {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if ext := filepath.Ext(p); !d.IsDir() && (ext == ".js" || ext == ".html" || ext == ".webmanifest") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	docs, err := filepath.Glob("../../docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		files = append(files, filepath.ToSlash(d))
	}
	files = append(files, "../../README.md")
	if len(files) < 30 {
		t.Fatalf("found %d files: the walk no longer reaches the UI and docs", len(files))
	}
	used := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, h := range vocabHits(f, string(b), used) {
			t.Errorf("%s:%s", f, h)
		}
	}
	for i, a := range vocabAllow {
		for _, w := range a.words {
			if !used[allowKey(i, w)] {
				t.Errorf("allow-list entry %s %q lets %q through but matched nothing: remove it", a.file, a.line, w)
			}
		}
	}
}

// 038-AC16: the AI host app's Activity hint names a tab the app has, and README names the room's
// menu by the room's name. Raw text: the vocabulary scan blanks the code spans these sit in.
func TestPlacesNamedExist(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	hint := regexp.MustCompile(`for the days set on (\w+)`).FindStringSubmatch(read("../daemon/ui/monitor.js"))
	tabs := regexp.MustCompile(`const tabs = (\[.*\]);`).FindStringSubmatch(read("../daemon/ui/admin.js"))
	if hint == nil || tabs == nil {
		t.Fatal("the Activity hint or the tabs list moved: update this test")
	}
	if !strings.Contains(tabs[1], "'"+hint[1]+"']") {
		t.Errorf("the Activity hint names %q, not a tab of %s", hint[1], tabs[1])
	}
	readme := read("../../README.md")
	for _, old := range []string{"room page's `⋯`", "room's `⋯` menu", "(`⋯` › Personas)"} {
		if strings.Contains(readme, old) {
			t.Errorf("README names the room's menu as %q: name it by the room's name", old)
		}
	}
	// 042: a "Room settings › <tab>" shown in the UI names one of the sheet's tabs, whole.
	all := regexp.MustCompile(`(?s)const allTabs = \[(.*?)\];`).FindStringSubmatch(read("ui/admin/room-admin.js"))
	panel := regexp.MustCompile(`roomTabs = \{([^}]*)\}`).FindStringSubmatch(read("ui/admin/room-panel.js"))
	if all == nil || panel == nil {
		t.Fatal("the Room settings tab lists moved: update this test")
	}
	var sheetTabs []string
	for _, m := range regexp.MustCompile(`\['[a-z]+', '([^']+)'\]`).FindAllStringSubmatch(all[1], -1) {
		sheetTabs = append(sheetTabs, m[1])
	}
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(panel[1], -1) {
		sheetTabs = append(sheetTabs, m[1])
	}
	// Pinned: a renamed tab fails here first, and every mention of its old name has to follow.
	pinned := []string{"Links", "Personas", "Skills", "Plugins", "Websites and connectors", "Deleted documents", "Words to recognise",
		"Call voices", "General", "Invites", "Members and admins", "Export"}
	if strings.Join(sheetTabs, "|") != strings.Join(pinned, "|") {
		t.Errorf("Room settings tabs = %q, pinned %q: update the pin and every \"Room settings › <old name>\"", sheetTabs, pinned)
	}
	// nameOf reads a mention as the longest tab name it starts with, pinned names included: a
	// mention of a renamed tab's old name then shows on the run that breaks the pin.
	names := append(slices.Clone(sheetTabs), pinned...)
	nameOf := func(rest string) string {
		best := ""
		for _, tab := range names {
			r, _ := utf8.DecodeRuneInString(rest[min(len(tab), len(rest)):])
			if len(tab) > len(best) && strings.HasPrefix(rest, tab) && (len(rest) == len(tab) || !unicode.IsLetter(r)) {
				best = tab
			}
		}
		return best
	}
	// Any run of spaces, a wrap or &nbsp; between the words, as phrase() allows; › or its entities.
	gap := `(?:\s|&nbsp;|\x{a0})`
	named := regexp.MustCompile(`Room` + gap + `+settings` + gap + `+(?:›|&rsaquo;|&#8250;|&#x203[aA];)` + gap + `*`)
	check := func(p string, spans []vspan) {
		for _, sp := range spans {
			// Each mention from its own start, the line's later ones too; a wrap inside a name folds.
			for _, loc := range named.FindAllStringIndex(sp.text, -1) {
				rest := sp.text[loc[1]:]
				rest = strings.Join(strings.Fields(strings.ReplaceAll(rest[:min(len(rest), 80)], "&nbsp;", " ")), " ")
				if n := nameOf(rest); !slices.Contains(sheetTabs, n) {
					t.Errorf("%s names %q, not a tab of Room settings %q", p, "Room settings › "+rest, sheetTabs)
				}
			}
		}
	}
	err := filepath.WalkDir("ui", func(p string, d fs.DirEntry, err error) error {
		if err != nil || vocabSkip[filepath.ToSlash(p)] {
			if err == nil && d.IsDir() {
				return filepath.SkipDir
			}
			return err
		}
		switch filepath.Ext(p) {
		case ".js":
			check(p, jsSpans(read(p)))
		case ".html":
			check(p, htmlSpans(read(p)))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	docs, err := filepath.Glob("../../docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(docs, "../../README.md") {
		check(p, mdSpans(read(p)))
	}
	// The bare "⋯ › <item>" form, for the room menu's own items; the people menu's stay.
	roomItem := regexp.MustCompile("⋯`? › \"?(Invite people|Room settings|Personas|Open (documents|chat) in a new window|Run the AI on my computer|Open my AI host app|Linked rooms|Link a room)")
	if m := roomItem.FindString(readme); m != "" {
		t.Errorf("README names the room's menu as %q: name it by the room's name", m)
	}
}

// 042-AC10 042-AC11: Help's words sit in room.html as a <dl>, each term in a <dt>, capitalised,
// in the spec's order, where the vocabulary scan reads them: a replaced word written there as a
// lone lowercase term, the shape the JS scan misses, fails it.
func TestHelpWords(t *testing.T) {
	b, err := os.ReadFile("ui/room.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	dl := regexp.MustCompile(`(?s)<dl id="help-words"[^>]*>.*?</dl>`).FindString(page)
	if dl == "" {
		t.Fatal(`room.html has no <dl id="help-words">`)
	}
	var terms, got []string
	for _, m := range regexp.MustCompile(`<dt>([^<]*)</dt>\s*<dd>([^<]+)</dd>`).FindAllStringSubmatch(dl, -1) {
		terms = append(terms, m[1])
		got = append(got, m[1]+": "+m[2])
	}
	// AC10's lines, the definition capitalised as a sentence of its own.
	want := []string{
		"Room: A place where people and personas talk, with its chat, documents and call.",
		"Persona: An AI with a name, a prompt and a model, who answers in the room when asked or on its own.",
		"AI host: The person whose computer runs the personas.",
		"AI host app: The app they run for it.",
		"Call: The room's audio, and its cameras and screens.",
		"Voice-to-text: What writes the call down as lines in the chat.",
		"Note: A comment pinned to a part of a document.",
		"Starter: A saved room setup (agenda, personas, skills) to start new rooms the same way.",
		"Skill: A set of instructions and files a persona can use.",
		"Plugin: Code from a git repository that gives a persona tools, run by the AI host.",
		"Linked room: Another room whose chat and documents this room's personas can read.",
		"Room admin: A member who manages the room, its members and invites.",
		"Server admin: The person who runs this grill-room server.",
	}
	if strings.Count(dl, "<dt") != len(want) || strings.Count(dl, "<dd") != len(want) {
		t.Errorf("Help's words hold %d <dt> and %d <dd>, want %d of each", strings.Count(dl, "<dt"), strings.Count(dl, "<dd"), len(want))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Help's words =\n%s\nwant\n%s\n(each a <dt> followed by its <dd>)", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, term := range terms {
		if term == "" || strings.ToUpper(term[:1]) != term[:1] {
			t.Errorf("term %q is not capitalised", term)
		}
	}
	if hits := vocabHits("ui/room.html", page, nil); len(hits) > 0 {
		t.Errorf("room.html: %v", hits)
	}
	// The red case: one term replaced by an old word, lowercase and alone.
	for _, old := range []string{"daemon", "annotation"} {
		bad := strings.Replace(page, "<dt>AI host app</dt>", "<dt>"+old+"</dt>", 1)
		if bad == page || len(vocabHits("ui/room.html", bad, nil)) == 0 {
			t.Errorf("a lone lowercase %q in Help's words passes the vocabulary scan", old)
		}
	}
}

// The scanner flags visible text only, in each shape the sources hold.
func TestVocabularyScanner(t *testing.T) {
	cases := []struct {
		name, file, src string
		hit             bool
	}{
		{"html text", "a.html", "<p>Start the daemon</p>", true},
		{"html text wrapped", "a.html", "<p>Mute the\n    room</p>", true},
		{"html nbsp", "a.html", "<p>Join&nbsp;audio</p>", true},
		{"html tip", "a.html", `<button data-tip="Join audio">x</button>`, true},
		{"html unquoted attr", "a.html", `<button title=Daemon>x</button>`, true},
		{"html value", "a.html", `<input value="Leave audio">`, true},
		{"html label", "a.html", `<optgroup label="Shared with everyone">`, true},
		{"html aria-description", "a.html", `<b aria-description="Room muted">x</b>`, true},
		{"html identifier value", "a.html", `<option value="daemon">AI host</option>`, false},
		// 042-AC11: a term is a text node, read even as a lone lowercase word.
		{"html lone lowercase term", "a.html", "<dl><dt>daemon</dt><dd>x</dd></dl>", true},
		{"html class", "a.html", `<div class="daemon-pill">AI host</div>`, false},
		{"html inline script", "a.html", "<script>toast('Room muted')</script>", true},
		{"html script code", "a.html", "<script>daemon()</script>", false},
		{"html comment", "a.html", "<!--\n the daemon\n-->", false},
		{"js string", "a.js", `toast('Mute the room')`, true},
		{"js template text", "a.js", "x = `${n} annotations`", true},
		{"js nested template", "a.js", "html`<p>${ok ? html`<b>Room muted</b>` : ''}</p>`", true},
		{"js template value", "a.js", "html`<input value=\"Join audio\">`", true},
		{"js template unquoted title", "a.js", "html`<b title=Daemon>x</b>`", true},
		{"js identifier", "a.js", "if (m.type === 'daemon_status') daemon.go()", false},
		{"js path", "a.js", "fetch(`/api/daemon/${id}`)", false},
		{"js class attr", "a.js", "html`<span class=\"daemon x\">AI host</span>`", false},
		{"js comment", "a.js", "// the daemon pairs\n/* deafened */", false},
		{"js regex", "a.js", "s.replace(/'/g, '') + ' AI host'", false},
		{"js regex after return", "a.js", "function f(s) { return /'/.test(s) }\ntoast('Room muted')", true},
		{"js regex after brace", "a.js", "if (a) {}\n/'/.test(s); toast('Room muted')", true},
		{"js division", "a.js", "x = a / b; toast('AI host') / 2", false},
		// ponytail: a lone lowercase word reads as an identifier, the scanner's known miss.
		{"js lone lowercase word", "a.js", `label = 'deafened'`, false},
		{"bare everyone", "a.js", `'Everyone on this server'`, true},
		{"bare shared", "a.js", `'Shared with everyone'`, true},
		{"full shared", "a.js", `'Shared with everyone on this server'`, false},
		{"approval mode text", "a.js", `hint('while the host approves everything')`, true},
		{"approval mode ids", "a.html", `<input id="approve-all"><script>s.approve_all = $('approve-all').checked</script>`, false},
		{"md prose", "a.md", "Run the daemon.\n", true},
		{"md wrapped phrase", "a.md", "Press Join\naudio to talk.\n", true},
		{"md wrapped full shared", "a.md", "Personas Shared with\neveryone on this server.\n", false},
		{"md paragraphs apart", "a.md", "Shared with\n\neveryone on this server.\n", true},
		{"md code span", "a.md", "Run `grill-room-daemon`.\n", false},
		{"md fence", "a.md", "```\ndaemon\n```\n", false},
		{"md comment", "a.md", "<!--\nthe daemon\n-->\n", false},
		{"md link target", "a.md", "See [the protocol](docs/daemon-protocol.md).\n", false},
		{"manifest name", "a.webmanifest", `{"name": "grill-room daemon"}`, true},
		{"manifest other key", "a.webmanifest", `{"theme_color": "the daemon"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := len(vocabHits(c.file, c.src, nil)) > 0; got != c.hit {
				t.Errorf("hit = %v, want %v", got, c.hit)
			}
		})
	}
}
