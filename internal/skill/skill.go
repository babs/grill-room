// Package skill validates Claude Code skill folders uploaded as zips (spec 002, AC29) and packs
// the skills a persona has active into the plugin directory claude -p loads with --plugin-dir
// (AC31). The room server uses it; the daemon only unpacks what it is sent.
package skill

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/babs/grill-room/internal/turn"
)

// Limits on an upload. Every member of every room may upload, so they bound what one person can
// make the server store and every host's sandbox load.
const (
	MaxZipBytes    = 8 << 20
	MaxTotalBytes  = 8 << 20
	MaxFiles       = 200
	MaxSkillMD     = 256 << 10
	MaxDescription = 1024
	MaxPathLen     = 200
	// manifest is the file every skill folder holds at its root.
	manifest = "SKILL.md"
)

// ErrInvalid wraps every reason an upload is refused; the message says which.
var ErrInvalid = errors.New("invalid skill")

// nameRe is Claude Code's rule for a skill name: lowercase letters, digits and single hyphens.
var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidName reports whether name is a skill name Claude Code takes, as the frontmatter must hold.
func ValidName(name string) bool { return len(name) <= 64 && nameRe.MatchString(name) }

// Skill is a validated upload: its frontmatter, and its files as a normalised tar (sorted paths,
// fixed times, modes 0644 or 0755) whose hash tells two identical uploads apart.
type Skill struct {
	Name        string
	Description string
	Files       int
	Size        int64
	SHA256      string
	Tar         []byte
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

type file struct {
	name string
	exec bool
	data []byte
}

// Parse validates a zip holding one skill folder, SKILL.md at its root or in its single top
// folder, and returns it normalised. It refuses symlinks, absolute, non-canonical or ".." paths,
// and anything past the limits, whatever the zip headers claim.
func Parse(zipped []byte) (*Skill, error) {
	if len(zipped) > MaxZipBytes {
		return nil, invalid("the zip is over %d MB", MaxZipBytes>>20)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		return nil, invalid("not a zip: %v", err)
	}
	var files []file
	seen := map[string]bool{}
	var total int64
	for _, f := range zr.File {
		name, skip, err := entryName(f)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		if seen[strings.ToLower(name)] {
			return nil, invalid("%q is in the zip twice", name)
		}
		seen[strings.ToLower(name)] = true
		if len(files) == MaxFiles {
			return nil, invalid("more than %d files", MaxFiles)
		}
		left := MaxTotalBytes - total
		if f.UncompressedSize64 > uint64(left) { //nolint:gosec // left is positive and small
			return nil, invalid("over %d MB once unpacked", MaxTotalBytes>>20)
		}
		data, err := readEntry(f, left)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		files = append(files, file{name: name, exec: f.Mode()&0o111 != 0, data: data})
	}
	files, err = strip(files)
	if err != nil {
		return nil, err
	}
	sk := &Skill{Files: len(files), Size: total}
	for _, f := range files {
		if f.name == manifest {
			if sk.Name, sk.Description, err = frontmatter(f.data); err != nil {
				return nil, err
			}
		}
	}
	if sk.Tar, err = pack(files); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(sk.Tar)
	sk.SHA256 = hex.EncodeToString(sum[:])
	return sk, nil
}

// entryName checks a zip entry's name and type. skip is true for directories and the litter
// macOS Finder adds to every zip it makes.
func entryName(f *zip.File) (name string, skip bool, err error) {
	name = f.Name
	mode := f.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		return "", false, invalid("%q is a symlink", name)
	case strings.HasPrefix(name, "__MACOSX/") || path.Base(name) == ".DS_Store":
		return "", true, nil
	case !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl):
		return "", false, invalid("a path holds a control character or is not UTF-8")
	case strings.Contains(name, `\`):
		return "", false, invalid("%q uses backslashes", name)
	case strings.HasPrefix(name, "/"):
		return "", false, invalid("%q is an absolute path", name)
	case len(name) > MaxPathLen:
		return "", false, invalid("a path is over %d characters", MaxPathLen)
	}
	clean := strings.TrimSuffix(name, "/")
	for _, el := range strings.Split(clean, "/") {
		if el == ".." {
			return "", false, invalid("%q leaves the skill folder", name)
		}
	}
	if path.Clean(clean) != clean {
		return "", false, invalid("%q is not a plain relative path", name)
	}
	if mode.IsDir() || strings.HasSuffix(name, "/") {
		return "", true, nil
	}
	if !mode.IsRegular() {
		return "", false, invalid("%q is not a regular file", name)
	}
	return clean, false, nil
}

// readEntry reads an entry up to left bytes: the header's size is the uploader's word, the
// decompressed bytes are not.
func readEntry(f *zip.File, left int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, invalid("%q: %v", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, left+1))
	if err != nil {
		return nil, invalid("%q: %v", f.Name, err)
	}
	if int64(len(data)) > left {
		return nil, invalid("over %d MB once unpacked", MaxTotalBytes>>20)
	}
	return data, nil
}

// strip finds the skill folder: SKILL.md at the zip's root, or a single top folder holding it,
// which it removes from every path.
func strip(files []file) ([]file, error) {
	if len(files) == 0 {
		return nil, invalid("the zip is empty")
	}
	for _, f := range files {
		if f.name == manifest {
			return files, nil
		}
	}
	top, _, _ := strings.Cut(files[0].name, "/")
	for i, f := range files {
		rest, ok := strings.CutPrefix(f.name, top+"/")
		if !ok {
			return nil, invalid("no %s at the root of the zip or of its single folder", manifest)
		}
		files[i].name = rest
	}
	for _, f := range files {
		if f.name == manifest {
			return files, nil
		}
	}
	return nil, invalid("no %s at the root of the zip or of its single folder", manifest)
}

// frontmatterSpan finds SKILL.md's YAML header in text: it runs from start, after the opening
// "---" line, to end, before the first "\n---"; start is -1 without an opening line, ok false
// without a closing one. LF and CRLF alike.
func frontmatterSpan(text string) (start, end int, ok bool) {
	switch {
	case strings.HasPrefix(text, "---\n"):
		start = 4
	case strings.HasPrefix(text, "---\r\n"):
		start = 5
	default:
		return -1, 0, false
	}
	i := strings.Index(text[start:], "\n---")
	if i < 0 {
		return start, 0, false
	}
	// A CRLF header's last line keeps its \r: the caller drops it with the other ones.
	return start, start + i, true
}

// frontmatter reads SKILL.md's YAML header and checks the two fields Claude Code needs.
func frontmatter(md []byte) (name, description string, err error) {
	if len(md) > MaxSkillMD {
		return "", "", invalid("%s is over %d KB", manifest, MaxSkillMD>>10)
	}
	start, end, ok := frontmatterSpan(string(md))
	switch {
	case start < 0:
		return "", "", invalid("%s must start with a --- frontmatter block", manifest)
	case !ok:
		return "", "", invalid("%s frontmatter has no closing ---", manifest)
	}
	head := strings.TrimSuffix(strings.ReplaceAll(string(md)[start:end], "\r\n", "\n"), "\r")
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		return "", "", invalid("%s frontmatter: %v", manifest, err)
	}
	fm.Description = strings.TrimSpace(fm.Description)
	switch {
	case !ValidName(fm.Name):
		return "", "", invalid("frontmatter name must be 1 to 64 lowercase letters, digits and hyphens")
	case fm.Description == "" || utf8.RuneCountInString(fm.Description) > MaxDescription:
		return "", "", invalid("frontmatter description must be 1 to %d characters", MaxDescription)
	}
	return fm.Name, fm.Description, nil
}

// epoch stamps every packed file: the same files always make the same bytes.
var epoch = time.Unix(0, 0).UTC()

func pack(files []file) ([]byte, error) {
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		mode := int64(0o644)
		if f.exec {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: mode, Size: int64(len(f.data)), ModTime: epoch}); err != nil {
			return nil, fmt.Errorf("pack skill: %w", err)
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, fmt.Errorf("pack skill: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("pack skill: %w", err)
	}
	return buf.Bytes(), nil
}

// Packed is a skill as the store keeps it: its name and its normalised tar.
type Packed struct {
	Name string
	Tar  []byte
}

// WritePlugin writes the plugin directory Claude Code loads with --plugin-dir, as a tar:
// .claude-plugin/plugin.json and skills/<name>/ for each skill (checked against Claude Code
// 2.1.296 with `claude plugin validate` and `claude --plugin-dir <dir> plugin details`).
func WritePlugin(w io.Writer, skills []Packed) error {
	tw := tar.NewWriter(w)
	manifest, err := json.Marshal(map[string]string{"name": turn.PluginName, "version": "1.0.0",
		"description": "The skills active for this persona in its grill-room room."})
	if err != nil {
		return fmt.Errorf("plugin manifest: %w", err)
	}
	dir := func(name string) error {
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: epoch})
	}
	for _, d := range []string{".claude-plugin", "skills"} {
		if err := dir(d); err != nil {
			return fmt.Errorf("plugin: %w", err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: ".claude-plugin/plugin.json", Mode: 0o644, Size: int64(len(manifest)), ModTime: epoch}); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	if _, err := tw.Write(manifest); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	for _, sk := range skills {
		if err := copySkill(tw, sk, dir); err != nil {
			return err
		}
	}
	return tw.Close()
}

// copySkill re-roots one skill's tar under skills/<name>/, its folders created first.
func copySkill(tw *tar.Writer, sk Packed, dir func(string) error) error {
	if !nameRe.MatchString(sk.Name) {
		return fmt.Errorf("plugin: bad skill name %q", sk.Name)
	}
	root := "skills/" + sk.Name
	made := map[string]bool{}
	mkdirs := func(p string) error {
		var parts []string
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			parts = append(parts, d)
		}
		for i := len(parts) - 1; i >= 0; i-- {
			if !made[parts[i]] {
				made[parts[i]] = true
				if err := dir(root + "/" + parts[i]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := dir(root); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	tr := tar.NewReader(bytes.NewReader(sk.Tar))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("plugin: skill %s: %w", sk.Name, err)
		}
		if err := mkdirs(h.Name); err != nil {
			return fmt.Errorf("plugin: %w", err)
		}
		var body io.Reader = tr
		if h.Name == manifest {
			md, err := io.ReadAll(io.LimitReader(tr, MaxSkillMD+1))
			if err != nil {
				return fmt.Errorf("plugin: skill %s: %w", sk.Name, err)
			}
			// A header the rename cannot rewrite keeps its old name: a mismatch beats failing every turn.
			if renamed, err := withName(md, sk.Name); err == nil {
				md = renamed
			}
			h.Size, body = int64(len(md)), bytes.NewReader(md)
		}
		h.Name = root + "/" + h.Name
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("plugin: %w", err)
		}
		if _, err := io.Copy(tw, body); err != nil { //nolint:gosec // the store's own tar, sized when it was parsed
			return fmt.Errorf("plugin: %w", err)
		}
	}
}

// withName returns md with its frontmatter name set to name. A merge renames a skill row (x to x-2)
// without touching its stored files, and the agent must see the row's name (011-AC11). An
// unrenamed skill comes back byte for byte; a header whose name line cannot be rewritten to read
// back as name is an error, and the caller keeps md as it is.
func withName(md []byte, name string) ([]byte, error) {
	if n, _, err := frontmatter(md); err == nil && n == name {
		return md, nil
	}
	text := string(md)
	start, end, ok := frontmatterSpan(text)
	if !ok || start < 0 {
		return nil, fmt.Errorf("SKILL.md has no frontmatter to name %s", name)
	}
	lines := strings.SplitAfter(text[start:end], "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "name:") {
			continue
		}
		eol := l[len(strings.TrimRight(l, "\r\n")):]
		lines[i] = "name: " + name + eol
		// A block scalar's indented lines belong to the old name.
		j := i + 1
		for j < len(lines) && (strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t")) {
			j++
		}
		lines = append(lines[:i+1], lines[j:]...)
		break
	}
	out := []byte(text[:start] + strings.Join(lines, "") + text[end:])
	if n, _, err := frontmatter(out); err != nil || n != name {
		return nil, fmt.Errorf("cannot set the SKILL.md frontmatter name to %s", name)
	}
	return out, nil
}

// ParseTar validates a skill's normalised tar, as the store keeps it and an import carries it,
// with the rules of Parse, and requires it normalised: the store's copy hashes to its sha256, and
// the plugin is built from it.
func ParseTar(data []byte) (*Skill, error) {
	if len(data) > MaxZipBytes {
		return nil, invalid("the skill is over %d MB", MaxZipBytes>>20)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, invalid("not a tar: %v", err)
		}
		zh := &zip.FileHeader{Name: h.Name, Method: zip.Store}
		zh.SetMode(h.FileInfo().Mode())
		w, err := zw.CreateHeader(zh)
		if err != nil {
			return nil, invalid("%q: %v", h.Name, err)
		}
		if _, err := io.Copy(w, tr); err != nil { //nolint:gosec // bounded by the tar's own size, checked above
			return nil, invalid("%q: %v", h.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, invalid("repack: %v", err)
	}
	sk, err := Parse(buf.Bytes())
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(sk.Tar, data) {
		return nil, invalid("the skill's tar is not in normalised form")
	}
	return sk, nil
}
