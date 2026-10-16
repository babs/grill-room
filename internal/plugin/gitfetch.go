package plugin

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1" //nolint:gosec // git object ids are SHA-1; integrity comes from walking ids down from the pinned commit
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	// ErrNoAccess means the repo needs credentials or does not exist: the host may fetch it (009-AC19).
	ErrNoAccess = errors.New("the server cannot fetch this repo anonymously")
	// ErrNoV2 refuses a git server that answers in protocol v0 or as a dumb server.
	ErrNoV2 = errors.New("this git server does not speak protocol v2")
	// ErrTooBig wraps every size refusal; the message names the limit.
	ErrTooBig = errors.New("too big")
)

// Kind is what a tree entry is.
type Kind uint8

// Tree entry kinds, from the git mode.
const (
	Regular Kind = iota
	Executable
	Symlink
	Submodule
)

// Entry is one non-directory entry of a commit's tree. Data is the file's bytes or the symlink's
// target; nil for a submodule.
type Entry struct {
	Path string
	Kind Kind
	Data []byte
}

const (
	userAgent = "git/2.0 (grill-room)"
	// maxRefsBody bounds the ref listing and capability answers, filtered by prefix and small.
	maxRefsBody = 4 << 20
	// inflateFactor bounds the unpacked objects of a fetch, and the files its tree walk lists, to
	// this many times its byte cap: the pack holds the whole repo at the commit, not only the plugin.
	inflateFactor = 4
	// maxObjects bounds a pack's objects and a tree walk's entries, whatever their sizes.
	maxObjects = 100_000
	// maxNameLen is the longest file name the common file systems take.
	maxNameLen = 255
)

var (
	hexRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	refRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,254}$`)
)

// Resolve turns ref into a commit: empty is the default branch, else a tag (peeled) or branch of
// that name, tags first as git does, or a full commit hash taken as is (Fetch verifies it). name
// is what the room shows: the ref, or the default branch's name.
func Resolve(ctx context.Context, c *http.Client, repoURL, ref string) (commit, name string, err error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if err := CheckURL(repoURL); err != nil {
		return "", "", err
	}
	if hexRe.MatchString(ref) {
		return ref, ref, nil
	}
	var prefixes []string
	switch {
	case ref == "":
		prefixes = []string{"HEAD"}
	case !refRe.MatchString(ref) || strings.Contains(ref, ".."):
		return "", "", fmt.Errorf("%q is not a valid branch, tag or commit", ref)
	case strings.HasPrefix(ref, "refs/"):
		prefixes = []string{ref}
	default:
		prefixes = []string{"refs/tags/" + ref, "refs/heads/" + ref}
	}
	base, err := discover(ctx, c, repoURL)
	if err != nil {
		return "", "", err
	}
	args := []string{"symrefs", "peel"}
	for _, p := range prefixes {
		args = append(args, "ref-prefix "+p)
	}
	resp, err := post(ctx, c, base, command("ls-refs", args))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	refs, err := readRefs(io.LimitReader(resp.Body, maxRefsBody))
	if err != nil {
		return "", "", err
	}
	for _, p := range prefixes {
		r, ok := refs[p]
		if !ok {
			continue
		}
		if ref != "" {
			return r.oid, ref, nil
		}
		name = "HEAD"
		if b, ok := strings.CutPrefix(r.target, "refs/heads/"); ok {
			name = b
		}
		return r.oid, name, nil
	}
	if ref == "" {
		return "", "", errors.New("the repo has no default branch")
	}
	return "", "", fmt.Errorf("no branch or tag %q in the repo", ref)
}

type lsRef struct{ oid, target string }

// readRefs parses an ls-refs answer: "<oid> <name>[ symref-target:<t>][ peeled:<oid>]".
func readRefs(r io.Reader) (map[string]lsRef, error) {
	pr := &pktReader{r: r}
	refs := map[string]lsRef{}
	for {
		kind, line, err := pr.line()
		if err != nil {
			return nil, err
		}
		if kind == pktFlush {
			return refs, nil
		}
		f := strings.Fields(string(line))
		if len(f) < 2 || !hexRe.MatchString(f[0]) {
			return nil, fmt.Errorf("git server: unexpected ref line %q", truncate(line))
		}
		ref := lsRef{oid: f[0]}
		for _, a := range f[2:] {
			if v, ok := strings.CutPrefix(a, "peeled:"); ok && hexRe.MatchString(v) {
				ref.oid = v
			} else if v, ok := strings.CutPrefix(a, "symref-target:"); ok {
				ref.target = v
			}
		}
		refs[f[1]] = ref
	}
}

// Fetch downloads the tree of commit, shallow, and returns its files sorted by path. It reads at
// most maxBytes of the server's answer and aborts past it (009-AC9).
func Fetch(ctx context.Context, c *http.Client, repoURL, commit string, maxBytes int64) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	objs, _, err := fetchObjects(ctx, c, repoURL, commit, maxBytes, true)
	if err != nil {
		return nil, err
	}
	return walk(ctx, objs, commit, maxBytes)
}

// fetchTimeout is Timeout; tests shorten it.
var fetchTimeout = Timeout

func fetchObjects(ctx context.Context, c *http.Client, repoURL, commit string, maxBytes int64, ofsDelta bool) (map[oid]*object, packStats, error) {
	var st packStats
	if err := CheckURL(repoURL); err != nil {
		return nil, st, err
	}
	if !hexRe.MatchString(commit) {
		return nil, st, fmt.Errorf("%q is not a full commit hash", commit)
	}
	base, err := discover(ctx, c, repoURL)
	if err != nil {
		return nil, st, err
	}
	args := []string{"no-progress", "deepen 1", "want " + commit, "done"}
	if ofsDelta {
		args = append([]string{"ofs-delta"}, args...)
	}
	resp, err := post(ctx, c, base, command("fetch", args))
	if err != nil {
		return nil, st, err
	}
	defer resp.Body.Close()
	pr := &pktReader{r: &capReader{r: resp.Body, left: maxBytes, max: maxBytes}}
	for {
		kind, line, err := pr.line()
		if err != nil {
			return nil, st, err
		}
		if kind == pktFlush {
			return nil, st, errors.New("git server: no packfile in the answer")
		}
		if kind == pktData && string(line) == "packfile" {
			break
		}
	}
	return parsePack(ctx, &sideband{pr: pr}, maxBytes)
}

// discover checks the server speaks protocol v2 and returns the repo URL to post to, after the
// redirects the server asked for.
func discover(ctx context.Context, c *http.Client, repoURL string) (string, error) {
	repoURL = strings.TrimRight(repoURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repoURL+"/info/refs?service=git-upload-pack", http.NoBody)
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}
	defer resp.Body.Close()
	if err := status(resp); err != nil {
		return "", err
	}
	if resp.Header.Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
		return "", ErrNoV2
	}
	pr := &pktReader{r: io.LimitReader(resp.Body, maxRefsBody)}
	_, line, err := pr.line()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(string(line), "# service=") {
		if kind, _, err := pr.line(); err != nil || kind != pktFlush {
			return "", ErrNoV2
		}
		if _, line, err = pr.line(); err != nil {
			return "", err
		}
	}
	if string(line) != "version 2" {
		return "", ErrNoV2
	}
	u := *resp.Request.URL
	u.RawQuery = ""
	u.Path = strings.TrimSuffix(u.Path, "/info/refs")
	u.RawPath = ""
	return u.String(), nil
}

func post(ctx context.Context, c *http.Client, base string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/git-upload-pack", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}
	if err := status(resp); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func status(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return ErrNoAccess
	}
	return fmt.Errorf("git server answered %s", resp.Status)
}

// command builds a protocol v2 request.
func command(name string, args []string) []byte {
	var b bytes.Buffer
	pkt(&b, "command="+name+"\n")
	pkt(&b, "agent="+userAgent+"\n")
	b.WriteString("0001")
	for _, a := range args {
		pkt(&b, a+"\n")
	}
	b.WriteString("0000")
	return b.Bytes()
}

func pkt(b *bytes.Buffer, s string) { fmt.Fprintf(b, "%04x%s", len(s)+4, s) }

const (
	pktData = iota
	pktFlush
	pktDelim
	pktEnd
)

// pktReader reads pkt-lines. The returned slice is valid until the next call.
type pktReader struct {
	r   io.Reader
	buf [65520]byte
}

func (p *pktReader) next() (int, []byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(p.r, h[:]); err != nil {
		return 0, nil, fmt.Errorf("git: reading the answer: %w", noEOF(err))
	}
	n, err := strconv.ParseUint(string(h[:]), 16, 16)
	if err != nil {
		return 0, nil, fmt.Errorf("git server: bad pkt-line length %q", h)
	}
	switch {
	case n < 3:
		return int(n) + pktFlush, nil, nil
	case n == 3 || n > uint64(len(p.buf)):
		return 0, nil, fmt.Errorf("git server: bad pkt-line length %d", n)
	}
	data := p.buf[:n-4]
	if _, err := io.ReadFull(p.r, data); err != nil {
		return 0, nil, fmt.Errorf("git: reading the answer: %w", noEOF(err))
	}
	if msg, ok := bytes.CutPrefix(data, []byte("ERR ")); ok {
		return 0, nil, fmt.Errorf("git server: %s", truncate(msg))
	}
	// Raw: a sideband packet of pack data may end in a 0x0a byte that belongs to the pack.
	return pktData, data, nil
}

// line reads a text pkt-line, without its trailing newline.
func (p *pktReader) line() (int, []byte, error) {
	kind, data, err := p.next()
	return kind, bytes.TrimSuffix(data, []byte("\n")), err
}

func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func truncate(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return strings.ToValidUTF8(string(b), "?")
}

// sideband demultiplexes band 1 (pack data) from progress (2) and errors (3).
type sideband struct {
	pr      *pktReader
	pending []byte
}

func (s *sideband) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		kind, data, err := s.pr.next()
		if err != nil {
			return 0, err
		}
		if kind != pktData {
			return 0, io.EOF
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case 1:
			s.pending = data[1:]
		case 2:
		case 3:
			return 0, fmt.Errorf("git server: %s", truncate(data[1:]))
		default:
			return 0, fmt.Errorf("git server: unknown sideband channel %d", data[0])
		}
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// capReader fails once more than max bytes were read.
type capReader struct {
	r         io.Reader
	left, max int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, tooBig("the repo", c.max)
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return 0, tooBig("the repo", c.max)
	}
	return n, err
}

func tooBig(what string, limit int64) error {
	if limit < 1<<20 {
		return fmt.Errorf("%w: %s is bigger than %d bytes", ErrTooBig, what, limit)
	}
	return fmt.Errorf("%w: %s is bigger than %d MB", ErrTooBig, what, limit>>20)
}

type oid [20]byte

func (o oid) String() string { return hex.EncodeToString(o[:]) }

const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

type object struct {
	typ  byte
	data []byte
}

// packStats counts what a pack held. pendingScans counts every look at a waiting REF_DELTA: it
// stays linear in their number.
type packStats struct{ objects, ofsDeltas, refDeltas, deltaApplies, pendingScans int }

// countReader counts what the pack parser consumed, for OFS_DELTA offsets. Its ReadByte keeps
// flate from reading past the end of each zlib stream.
type countReader struct {
	r *bufio.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// parsePack reads a version 2 or 3 packfile into objects keyed by id. Every object and every
// delta result is bounded by maxBytes, all of them together by inflateFactor times that, and
// their number by maxObjects.
func parsePack(ctx context.Context, r io.Reader, maxBytes int64) (map[oid]*object, packStats, error) {
	var st packStats
	cr := &countReader{r: bufio.NewReaderSize(r, 64<<10)}
	var hdr [12]byte
	if _, err := io.ReadFull(cr, hdr[:]); err != nil {
		return nil, st, fmt.Errorf("pack: %w", noEOF(err))
	}
	if string(hdr[:4]) != "PACK" || (hdr[7] != 2 && hdr[7] != 3) || hdr[4]|hdr[5]|hdr[6] != 0 {
		return nil, st, errors.New("pack: not a version 2 packfile")
	}
	count := int(hdr[8])<<24 | int(hdr[9])<<16 | int(hdr[10])<<8 | int(hdr[11])
	if count > maxObjects {
		return nil, st, fmt.Errorf("%w: the repo has more than %d objects", ErrTooBig, maxObjects)
	}
	// The count is the server's word: it never sizes an allocation on its own.
	objs := make(map[oid]*object, min(count, 1024))
	byOff := make(map[int64]*object, min(count, 1024))
	budget := maxBytes * inflateFactor
	// A REF_DELTA may name a base later in the pack: it waits, indexed by that base.
	pending := map[oid][][]byte{}
	npending := 0
	// put stores o, then every delta waiting on it, and on those, as a worklist: each delta is
	// applied once.
	put := func(o *object) error {
		work := []*object{o}
		for len(work) > 0 {
			o := work[len(work)-1]
			work = work[:len(work)-1]
			id := hashObject(o)
			objs[id] = o
			for _, delta := range pending[id] {
				st.pendingScans++
				d, err := applyDelta(o, delta, maxBytes, &budget)
				if err != nil {
					return err
				}
				st.deltaApplies++
				npending--
				work = append(work, d)
			}
			delete(pending, id)
		}
		return nil
	}
	var zr io.ReadCloser
	inflate := func(size int64) ([]byte, error) {
		if size > maxBytes {
			return nil, tooBig("a file of the repo", maxBytes)
		}
		if budget -= size; budget < 0 {
			return nil, tooBig("the repo once unpacked", maxBytes*inflateFactor)
		}
		var err error
		if zr == nil {
			zr, err = zlib.NewReader(cr)
		} else {
			err = zr.(zlib.Resetter).Reset(cr, nil)
		}
		if err != nil {
			return nil, fmt.Errorf("pack: %w", err)
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(zr, data); err != nil {
			return nil, fmt.Errorf("pack: %w", noEOF(err))
		}
		var one [1]byte
		if n, err := io.ReadFull(zr, one[:]); n != 0 || !errors.Is(err, io.EOF) {
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("pack: %w", err)
			}
			return nil, errors.New("pack: object longer than its header says")
		}
		return data, nil
	}
	for range count {
		if err := ctx.Err(); err != nil {
			return nil, st, fmt.Errorf("pack: %w", err)
		}
		off := cr.n
		typ, size, err := typeSize(cr)
		if err != nil {
			return nil, st, err
		}
		var o *object
		switch typ {
		case objCommit, objTree, objBlob, objTag:
			data, err := inflate(size)
			if err != nil {
				return nil, st, err
			}
			o = &object{typ: typ, data: data}
		case objOfsDelta:
			rel, err := ofs(cr)
			if err != nil {
				return nil, st, err
			}
			base := byOff[off-rel]
			if rel <= 0 || base == nil {
				return nil, st, errors.New("pack: delta against an unknown offset")
			}
			delta, err := inflate(size)
			if err != nil {
				return nil, st, err
			}
			if o, err = applyDelta(base, delta, maxBytes, &budget); err != nil {
				return nil, st, err
			}
			st.ofsDeltas++
		case objRefDelta:
			var id oid
			if _, err := io.ReadFull(cr, id[:]); err != nil {
				return nil, st, fmt.Errorf("pack: %w", noEOF(err))
			}
			delta, err := inflate(size)
			if err != nil {
				return nil, st, err
			}
			st.refDeltas++
			base := objs[id]
			if base == nil {
				pending[id] = append(pending[id], delta)
				npending++
				continue
			}
			if o, err = applyDelta(base, delta, maxBytes, &budget); err != nil {
				return nil, st, err
			}
			st.deltaApplies++
		default:
			return nil, st, fmt.Errorf("pack: unknown object type %d", typ)
		}
		byOff[off] = o
		if err := put(o); err != nil {
			return nil, st, err
		}
	}
	if npending > 0 {
		for id := range pending {
			return nil, st, fmt.Errorf("pack: delta base %s missing", id)
		}
	}
	st.objects = len(objs)
	return objs, st, nil
}

func typeSize(r io.ByteReader) (byte, int64, error) {
	c, err := r.ReadByte()
	if err != nil {
		return 0, 0, fmt.Errorf("pack: %w", noEOF(err))
	}
	typ := (c >> 4) & 7
	size := int64(c & 15)
	for shift := 4; c&0x80 != 0; shift += 7 {
		if shift > 53 {
			return 0, 0, errors.New("pack: object size overflows")
		}
		if c, err = r.ReadByte(); err != nil {
			return 0, 0, fmt.Errorf("pack: %w", noEOF(err))
		}
		size |= int64(c&0x7f) << shift
	}
	return typ, size, nil
}

func ofs(r io.ByteReader) (int64, error) {
	c, err := r.ReadByte()
	if err != nil {
		return 0, fmt.Errorf("pack: %w", noEOF(err))
	}
	off := int64(c & 0x7f)
	for c&0x80 != 0 {
		if off >= 1<<48 {
			return 0, errors.New("pack: delta offset overflows")
		}
		if c, err = r.ReadByte(); err != nil {
			return 0, fmt.Errorf("pack: %w", noEOF(err))
		}
		off = (off+1)<<7 | int64(c&0x7f)
	}
	return off, nil
}

func hashObject(o *object) oid {
	names := [...]string{objCommit: "commit", objTree: "tree", objBlob: "blob", objTag: "tag"}
	h := sha1.New() //nolint:gosec // git object ids
	h.Write([]byte(names[o.typ] + " " + strconv.Itoa(len(o.data)) + "\x00"))
	h.Write(o.data)
	var id oid
	copy(id[:], h.Sum(nil))
	return id
}

// applyDelta rebuilds an object from its base and a git delta, every copy bounds-checked.
func applyDelta(base *object, delta []byte, maxBytes int64, budget *int64) (*object, error) {
	bad := errors.New("pack: corrupt delta")
	i := 0
	varint := func() (int64, bool) {
		var v int64
		for shift := 0; i < len(delta) && shift < 63; shift += 7 {
			c := delta[i]
			i++
			v |= int64(c&0x7f) << shift
			if c&0x80 == 0 {
				return v, true
			}
		}
		return 0, false
	}
	src, ok1 := varint()
	dst, ok2 := varint()
	if !ok1 || !ok2 || src != int64(len(base.data)) {
		return nil, bad
	}
	if dst > maxBytes {
		return nil, tooBig("a file of the repo", maxBytes)
	}
	if *budget -= dst; *budget < 0 {
		return nil, tooBig("the repo once unpacked", maxBytes*inflateFactor)
	}
	out := make([]byte, 0, dst)
	for i < len(delta) {
		op := delta[i]
		i++
		switch {
		case op&0x80 != 0:
			var off, n int64
			for b := range 7 {
				if op&(1<<b) == 0 {
					continue
				}
				if i >= len(delta) {
					return nil, bad
				}
				if b < 4 {
					off |= int64(delta[i]) << (8 * b)
				} else {
					n |= int64(delta[i]) << (8 * (b - 4))
				}
				i++
			}
			if n == 0 {
				n = 0x10000
			}
			if off+n > int64(len(base.data)) || int64(len(out))+n > dst {
				return nil, bad
			}
			out = append(out, base.data[off:off+n]...)
		case op != 0:
			n := int(op)
			if i+n > len(delta) || int64(len(out)+n) > dst {
				return nil, bad
			}
			out = append(out, delta[i:i+n]...)
			i += n
		default:
			return nil, bad
		}
	}
	if int64(len(out)) != dst {
		return nil, bad
	}
	return &object{typ: base.typ, data: out}, nil
}

// walk lists the files of commit's tree, sorted by path. Two identical folders share one tree
// object, so a tree may be reached more than once; the entry and byte caps bound what a pack of
// trees naming the same subtree over and over can expand to.
func walk(ctx context.Context, objs map[oid]*object, commit string, maxBytes int64) ([]Entry, error) {
	var id oid
	if _, err := hex.Decode(id[:], []byte(commit)); err != nil {
		return nil, fmt.Errorf("%q is not a commit hash", commit)
	}
	c := objs[id]
	if c == nil || c.typ != objCommit {
		return nil, fmt.Errorf("commit %s is not in the server's answer", commit)
	}
	line, _, _ := bytes.Cut(c.data, []byte("\n"))
	tree, ok := bytes.CutPrefix(line, []byte("tree "))
	if !ok || len(tree) != 40 {
		return nil, fmt.Errorf("commit %s has no tree", commit)
	}
	if _, err := hex.Decode(id[:], tree); err != nil {
		return nil, fmt.Errorf("commit %s has no tree", commit)
	}
	w := &walker{ctx: ctx, objs: objs, entries: maxObjects, bytes: maxBytes * inflateFactor, maxBytes: maxBytes}
	if err := w.tree(id, "", 0); err != nil {
		return nil, err
	}
	sort.Slice(w.out, func(i, j int) bool { return w.out[i].Path < w.out[j].Path })
	return w.out, nil
}

type walker struct {
	ctx      context.Context
	objs     map[oid]*object
	entries  int
	bytes    int64
	maxBytes int64
	out      []Entry
}

func (w *walker) tree(id oid, dir string, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return fmt.Errorf("git: %w", err)
	}
	t := w.objs[id]
	if t == nil || t.typ != objTree {
		return fmt.Errorf("tree %s is not in the server's answer", id)
	}
	if depth > 64 {
		return fmt.Errorf("%s: folders nested too deep", dir)
	}
	for data := t.data; len(data) > 0; {
		if w.entries--; w.entries < 0 {
			return fmt.Errorf("%w: the repo has more than %d files and folders", ErrTooBig, maxObjects)
		}
		sp := bytes.IndexByte(data, ' ')
		nul := bytes.IndexByte(data, 0)
		if sp < 0 || nul < sp || nul+21 > len(data) {
			return fmt.Errorf("tree %s is corrupt", id)
		}
		// Checked before any copy or join: nested long names would otherwise allocate each
		// level's full path for every file below it.
		if nul-sp-1 > maxNameLen || len(dir)+nul-sp > maxPathLen {
			return invalid("%.100s...: a name or path is too long", dir)
		}
		mode, name := string(data[:sp]), string(data[sp+1:nul])
		var child oid
		copy(child[:], data[nul+1:nul+21])
		data = data[nul+21:]
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			return fmt.Errorf("tree %s holds the bad name %q", id, name)
		}
		p := path.Join(dir, name)
		if mode == "40000" {
			if err := w.tree(child, p, depth+1); err != nil {
				return err
			}
			continue
		}
		e := Entry{Path: p}
		switch mode {
		case "160000":
			e.Kind = Submodule
			w.out = append(w.out, e)
			continue
		case "120000":
			e.Kind = Symlink
		case "100755":
			e.Kind = Executable
		case "100644", "100664":
			e.Kind = Regular
		default:
			return fmt.Errorf("%s: unknown git mode %s", p, mode)
		}
		b := w.objs[child]
		if b == nil || b.typ != objBlob {
			return fmt.Errorf("%s: file %s is not in the server's answer", p, child)
		}
		if w.bytes -= int64(len(b.data)); w.bytes < 0 {
			return tooBig("the repo once unpacked", w.maxBytes*inflateFactor)
		}
		e.Data = b.data
		w.out = append(w.out, e)
	}
	return nil
}
