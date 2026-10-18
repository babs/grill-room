package plugin

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// gitEnv isolates the git CLI from the user's and the system's config.
func gitEnv(home string) []string {
	return []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z"}
}

type gitRepo struct {
	t    *testing.T
	dir  string
	home string
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", append([]string{"-c", "core.compression=1", "-c", "core.autocrlf=false", "-c", "core.symlinks=true"}, args...)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), gitEnv(r.home)...)
	out, err := cmd.CombinedOutput()
	require.NoError(r.t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func (r *gitRepo) write(name, body string) {
	r.t.Helper()
	p := filepath.Join(r.dir, filepath.FromSlash(name))
	require.NoError(r.t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(r.t, os.WriteFile(p, []byte(body), 0o644))
}

func (r *gitRepo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

type gitServer struct {
	bodies chan string
	srv    *httptest.Server
	root   string
	home   string
	client *http.Client
}

// newGitServer serves every bare repo under its root with git http-backend over TLS. The client
// reaches it as example.com, the name in httptest's certificate.
func newGitServer(t *testing.T) *gitServer {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	home := t.TempDir()
	out, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output()
	require.NoError(t, err)
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if runtime.GOOS == "windows" {
		backend += ".exe"
	}
	gs := &gitServer{root: t.TempDir(), home: home, bodies: make(chan string, 64)}
	backendHandler := &cgi.Handler{
		Path:       backend,
		Env:        append([]string{"GIT_PROJECT_ROOT=" + gs.root, "GIT_HTTP_EXPORT_ALL=1"}, gitEnv(home)...),
		InheritEnv: []string{"PATH", "SYSTEMROOT", "TEMP", "TMP"},
	}
	gs.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			select {
			case gs.bodies <- string(b):
			default:
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		backendHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(gs.srv.Close)
	gs.client = testClient(gs.srv, func(h string) []netip.Addr {
		if h == "example.com" {
			return []netip.Addr{loopback}
		}
		return nil
	})
	return gs
}

func (gs *gitServer) url(name string) string {
	_, port, _ := net.SplitHostPort(gs.srv.Listener.Addr().String())
	return "https://example.com:" + port + "/" + name
}

// newRepo returns a work repo; publish makes the served bare copy.
func (gs *gitServer) newRepo(t *testing.T) *gitRepo {
	r := &gitRepo{t: t, dir: t.TempDir(), home: gs.home}
	r.git("init", "-q", "-b", "main")
	return r
}

func (gs *gitServer) publish(r *gitRepo, name string) string {
	r.t.Helper()
	dst := filepath.Join(gs.root, name)
	_ = os.RemoveAll(dst)
	r.git("clone", "-q", "--bare", r.dir, dst)
	return gs.url(name)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), Timeout)
	t.Cleanup(cancel)
	return c
}

func byPath(es []Entry) map[string]Entry {
	m := map[string]Entry{}
	for _, e := range es {
		m[e.Path] = e
	}
	return m
}

// 009-AC5: a ref resolves to a commit; empty is the default branch.
func TestResolve(t *testing.T) {
	gs := newGitServer(t)
	r := gs.newRepo(t)
	r.write("a.txt", "one")
	first := r.commit("one")
	r.git("tag", "-a", "v1", "-m", "release")
	r.git("tag", "light")
	r.git("checkout", "-q", "-b", "dev")
	r.write("a.txt", "two")
	dev := r.commit("two")
	r.git("tag", "dev-tag")
	r.git("checkout", "-q", "main")
	u := gs.publish(r, "repo.git")

	for _, tc := range []struct{ ref, commit, name string }{
		{"", first, "main"},
		{"main", first, "main"},
		{"dev", dev, "dev"},
		{"v1", first, "v1"},
		{"light", first, "light"},
		{"refs/heads/dev", dev, "refs/heads/dev"},
		{dev, dev, dev},
	} {
		c, n, err := Resolve(ctx(t), gs.client, u, tc.ref)
		require.NoError(t, err, tc.ref)
		require.Equal(t, tc.commit, c, tc.ref)
		require.Equal(t, tc.name, n, tc.ref)
	}
	// Without the .git suffix: http-backend, like GitHub, takes both.
	_, _, err := Resolve(ctx(t), gs.client, strings.TrimSuffix(u, ".git")+".git/", "dev")
	require.NoError(t, err)

	for _, bad := range []string{"nope", "a b", "x\ncommand=fetch", "a..b", "-x"} {
		_, _, err := Resolve(ctx(t), gs.client, u, bad)
		require.Error(t, err, bad)
	}
	// The remote names its default branch: one no ref the room takes may hold stays HEAD.
	for _, branch := range []string{"café", "caf\xe9", "x+y"} {
		r := gs.newRepo(t)
		r.write("a.txt", "one")
		head := r.commit("one")
		r.git("branch", "-m", branch)
		c, n, err := Resolve(ctx(t), gs.client, gs.publish(r, "odd"+strconv.Itoa(len(branch))+".git"), "")
		require.NoError(t, err, branch)
		require.Equal(t, head, c, branch)
		require.Equal(t, "HEAD", n, branch)
	}
	_, _, err = Resolve(ctx(t), gs.client, gs.url("missing.git"), "")
	require.ErrorIs(t, err, ErrNoAccess)
	_, _, err = Resolve(ctx(t), gs.client, "http://example.com/x", "")
	require.ErrorIs(t, err, ErrNotHTTPS)
}

// 009-AC8, 009-AC9: a shallow fetch of the pinned commit gives its files, modes and links.
func TestFetch(t *testing.T) {
	gs := newGitServer(t)
	r := gs.newRepo(t)
	r.write("README.md", "old")
	old := r.commit("old")
	r.write("README.md", "hello\n")
	r.write("bin/tool", "#!/bin/sh\necho hi\n")
	r.write("deep/a/b/c.txt", "c")
	r.git("add", "-A")
	r.git("update-index", "--chmod=+x", "bin/tool")
	target := filepath.Join(r.home, "target")
	require.NoError(t, os.WriteFile(target, []byte("README.md"), 0o644))
	r.git("update-index", "--add", "--cacheinfo", "120000,"+r.git("hash-object", "-w", target)+",link")
	r.git("update-index", "--add", "--cacheinfo", "160000,"+old+",sub")
	r.git("commit", "-q", "-m", "new")
	head := r.git("rev-parse", "HEAD")
	u := gs.publish(r, "fetch.git")

	es, err := Fetch(ctx(t), gs.client, u, head, 20<<20)
	require.NoError(t, err)
	// 009-AC8: only the pinned commit, shallow.
	body := <-gs.bodies
	require.Contains(t, body, "command=fetch")
	require.Contains(t, body, "000ddeepen 1\n")
	require.Contains(t, body, "0032want "+head+"\n")
	m := byPath(es)
	require.Equal(t, "hello\n", string(m["README.md"].Data))
	require.Equal(t, Executable, m["bin/tool"].Kind)
	require.Equal(t, Regular, m["deep/a/b/c.txt"].Kind)
	require.Equal(t, Symlink, m["link"].Kind)
	require.Equal(t, "README.md", string(m["link"].Data))
	require.Equal(t, Submodule, m["sub"].Kind)
	require.Len(t, es, 5)

	es, err = Fetch(ctx(t), gs.client, u, old, 20<<20)
	require.NoError(t, err)
	require.Equal(t, "old", string(byPath(es)["README.md"].Data))

	_, err = Fetch(ctx(t), gs.client, u, strings.Repeat("0", 40), 20<<20)
	require.Error(t, err)
	_, err = Fetch(ctx(t), gs.client, u, "main", 20<<20)
	require.Error(t, err)
}

// 009-AC9: deltified objects, OFS_DELTA and REF_DELTA, rebuild the files byte for byte.
func TestFetchDeltas(t *testing.T) {
	gs := newGitServer(t)
	r := gs.newRepo(t)
	base := make([]byte, 64<<10)
	_, _ = rand.Read(base)
	text := func(i int) string {
		var b strings.Builder
		for l := range 2000 {
			b.WriteString(strings.Repeat("x", l%70))
			if l%97 == i {
				b.WriteString(" changed")
			}
			b.WriteString("\n")
		}
		return b.String()
	}
	for i := range 6 {
		r.write("doc.txt", text(i))
		r.write("copy/doc.txt", text(i+10))
		r.write("bin.dat", string(base)+strings.Repeat("v", i))
		r.write("bin2.dat", string(base)+"other")
		r.commit("v")
	}
	head := r.git("rev-parse", "HEAD")
	u := gs.publish(r, "delta.git")
	bare := &gitRepo{t: t, dir: filepath.Join(gs.root, "delta.git"), home: gs.home}
	bare.git("repack", "-adf", "--depth=50", "--window=50")

	for _, ofs := range []bool{true, false} {
		objs, st, err := fetchObjects(ctx(t), gs.client, u, head, 20<<20, ofs)
		require.NoError(t, err)
		if ofs {
			require.Positive(t, st.ofsDeltas)
		} else {
			require.Positive(t, st.refDeltas)
		}
		es, err := walk(ctx(t), objs, head, 20<<20)
		require.NoError(t, err)
		m := byPath(es)
		require.Equal(t, text(5), string(m["doc.txt"].Data))
		require.Equal(t, text(15), string(m["copy/doc.txt"].Data))
		require.Equal(t, string(base)+"vvvvv", string(m["bin.dat"].Data))
		require.Equal(t, string(base)+"other", string(m["bin2.dat"].Data))
	}
}

type countingRT struct {
	rt http.RoundTripper
	n  *atomic.Int64
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	if err == nil {
		resp.Body = countingBody{resp.Body, c.n}
	}
	return resp, err
}

// 009-AC9: a 21 MB plugin with a 20 MB cap aborts before the whole answer is read.
func TestFetchCap(t *testing.T) {
	gs := newGitServer(t)
	r := gs.newRepo(t)
	big := make([]byte, 21<<20)
	_, _ = rand.Read(big)
	r.write("big.bin", string(big))
	head := r.commit("big")
	u := gs.publish(r, "big.git")
	var n atomic.Int64
	c := *gs.client
	c.Transport = countingRT{c.Transport, &n}

	_, err := Fetch(ctx(t), &c, u, head, 20<<20)
	require.ErrorIs(t, err, ErrTooBig)
	require.Contains(t, err.Error(), "bigger than 20 MB")
	require.Less(t, n.Load(), int64(len(big)))

	// Files each under the cap but together over it trip the stream cap.
	r2 := gs.newRepo(t)
	for _, name := range []string{"a", "b", "c"} {
		b := make([]byte, 20<<10)
		_, _ = rand.Read(b)
		r2.write(name, string(b))
	}
	head = r2.commit("three")
	n.Store(0)
	_, err = Fetch(ctx(t), &c, gs.publish(r2, "three.git"), head, 50<<10)
	require.ErrorIs(t, err, ErrTooBig)
	require.Contains(t, err.Error(), "the repo is bigger than 51200 bytes")
	require.LessOrEqual(t, n.Load(), int64(60<<10))
}

func TestServerShapes(t *testing.T) {
	v0 := "001e# service=git-upload-pack\n0000" + "003f" + strings.Repeat("a", 40) + " HEAD\x00multi_ack\n0000"
	cases := map[string]struct {
		ctype, body string
		code        int
		want        error
	}{
		"v0":        {"application/x-git-upload-pack-advertisement", v0, 200, ErrNoV2},
		"dumb":      {"text/plain", "ref: refs/heads/main\n", 200, ErrNoV2},
		"no header": {"application/x-git-upload-pack-advertisement", "000dversion 1\n0000", 200, ErrNoV2},
		"auth":      {"text/plain", "", 401, ErrNoAccess},
		"forbidden": {"text/plain", "", 403, ErrNoAccess},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := testClient(srv, func(string) []netip.Addr { return []netip.Addr{loopback} })
			_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
			_, _, err := Resolve(ctx(t), c, "https://example.com:"+port+"/r", "")
			require.ErrorIs(t, err, tc.want)
		})
	}
	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		c := testClient(srv, func(string) []netip.Addr { return []netip.Addr{loopback} })
		_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
		_, _, err := Resolve(ctx(t), c, "https://example.com:"+port+"/r", "")
		require.ErrorContains(t, err, "502")
	})
}

func pktString(lines ...string) string {
	var b bytes.Buffer
	for _, l := range lines {
		switch l {
		case "":
			b.WriteString("0000")
		default:
			pkt(&b, l)
		}
	}
	return b.String()
}

func TestPktAndSideband(t *testing.T) {
	_, err := readRefs(strings.NewReader(pktString("garbage\n", "")))
	require.Error(t, err)
	_, err = readRefs(strings.NewReader("zzzz"))
	require.Error(t, err)
	_, err = readRefs(strings.NewReader("0003"))
	require.Error(t, err)
	_, err = readRefs(strings.NewReader(pktString("ERR access denied\n")))
	require.ErrorContains(t, err, "access denied")
	_, err = readRefs(strings.NewReader("00ff"))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	sb := &sideband{pr: &pktReader{r: strings.NewReader(pktString("\x02progress", "\x01data", "\x03boom"))}}
	b := make([]byte, 10)
	n, err := sb.Read(b)
	require.NoError(t, err)
	require.Equal(t, "data", string(b[:n]))
	_, err = sb.Read(b)
	require.ErrorContains(t, err, "boom")
	sb = &sideband{pr: &pktReader{r: strings.NewReader(pktString("\x05what"))}}
	_, err = sb.Read(b)
	require.ErrorContains(t, err, "unknown sideband channel 5")

	// Pack data keeps a trailing 0x0a, alone in its packet or after other bytes; text lines drop it.
	sb = &sideband{pr: &pktReader{r: strings.NewReader(pktString("\x01ab\n", "\x01\n", ""))}}
	got, err := io.ReadAll(sb)
	require.NoError(t, err)
	require.Equal(t, "ab\n\n", string(got))
	refs, err := readRefs(strings.NewReader(pktString(strings.Repeat("a", 40)+" refs/heads/main\n", "")))
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("a", 40), refs["refs/heads/main"].oid)
}

// 004b-AC15: a pack whose sideband packets end in 0x0a parses whole: the newline is pack data, not a line end.
// The random blobs of TestFetchDeltas hit this about once in a hundred runs.
func TestPackPacketsEndingInNewline(t *testing.T) {
	data := make([]byte, 8<<10)
	rng := mrand.New(mrand.NewChaCha8([32]byte{1}))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write(data)
	require.NoError(t, zw.Close())
	pack := []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01")
	size := len(data)
	pack = append(pack, byte(0x80|objBlob<<4|size&0x0f))
	for size >>= 4; size > 0; size >>= 7 {
		c := byte(size & 0x7f)
		if size > 0x7f {
			c |= 0x80
		}
		pack = append(pack, c)
	}
	pack = append(pack, z.Bytes()...)
	require.Greater(t, bytes.Count(pack, []byte("\n")), 1)
	// Cut after every 0x0a: each packet but maybe the last ends in one.
	var lines []string
	for rest := pack; len(rest) > 0; {
		i := bytes.IndexByte(rest, '\n') + 1
		if i == 0 {
			i = len(rest)
		}
		lines = append(lines, "\x01"+string(rest[:i]))
		rest = rest[i:]
	}
	objs, _, err := parsePack(t.Context(), &sideband{pr: &pktReader{r: strings.NewReader(pktString(append(lines, "")...))}}, 1<<20)
	require.NoError(t, err)
	require.Len(t, objs, 1)
	for _, o := range objs {
		require.Equal(t, data, o.data)
	}
}

func TestPackErrors(t *testing.T) {
	_, _, err := parsePack(t.Context(), strings.NewReader("NOPE00000000"), 1<<20)
	require.Error(t, err)
	_, _, err = parsePack(t.Context(), strings.NewReader("PACK"), 1<<20)
	require.Error(t, err)
	hdr := "PACK\x00\x00\x00\x02\x00\x00\x00\x01"
	_, _, err = parsePack(t.Context(), strings.NewReader(hdr+"\x50"), 1<<20) // type 5 does not exist
	require.ErrorContains(t, err, "unknown object type")
	_, _, err = parsePack(t.Context(), strings.NewReader(hdr+"\x6a\x05"), 1<<20) // OFS_DELTA to nowhere
	require.ErrorContains(t, err, "unknown offset")
	_, _, err = parsePack(t.Context(), strings.NewReader(hdr+"\xbf\xff\xff\xff\xff\xff\xff\xff\xff\x01"), 1<<20)
	require.ErrorContains(t, err, "overflows")
	_, _, err = parsePack(t.Context(), strings.NewReader(hdr+"\xbf\x7f"), 100)
	require.ErrorIs(t, err, ErrTooBig)
	_, _, err = parsePack(t.Context(), strings.NewReader(hdr+"\x70"+strings.Repeat("\x01", 20)), 1<<20)
	require.Error(t, err)
}

func TestApplyDelta(t *testing.T) {
	base := &object{typ: objBlob, data: []byte("hello world")}
	budget := int64(1 << 20)
	ok := []byte{11, 8, 0x90, 5, 3, '!', '!', '!'} // copy 5 from 0, insert "!!!"
	o, err := applyDelta(base, ok, 1<<20, &budget)
	require.NoError(t, err)
	require.Equal(t, "hello!!!", string(o.data))
	for name, d := range map[string][]byte{
		"src size":     {10, 8},
		"no dst":       {11},
		"copy past":    {11, 20, 0x91, 5, 20},
		"insert past":  {11, 5, 9, 'a'},
		"zero op":      {11, 1, 0},
		"short result": {11, 9, 0x90, 5},
		"cut copy":     {11, 5, 0x91},
	} {
		_, err := applyDelta(base, d, 1<<20, &budget)
		require.Error(t, err, name)
	}
	_, err = applyDelta(base, []byte{11, 0x80, 0x80, 0x80, 0x01}, 1<<20, &budget)
	require.ErrorIs(t, err, ErrTooBig)
}

// Opt-in: fetches the 009 fixture from GitHub (GRILL_ROOM_TEST_LIVE_GIT=1).
func TestLiveFixture(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_LIVE_GIT") != "1" {
		t.Skip("set GRILL_ROOM_TEST_LIVE_GIT=1 to fetch from github.com")
	}
	const repo, pin = "https://github.com/babs/skills", "53195d86d96aa2917ab23a9729c51d1b0118dc82"
	c := NewClient()
	start := time.Now()
	commit, name, err := Resolve(ctx(t), c, repo, "")
	require.NoError(t, err)
	t.Logf("default branch %s @ %s", name, commit)
	es, err := Fetch(ctx(t), c, repo, pin, 20<<20)
	require.NoError(t, err)
	mp, err := ReadMarketplace(es)
	require.NoError(t, err)
	require.NotNil(t, mp)
	e := mp.Plugins[0]
	require.True(t, e.Pickable, e.Reason)
	s, err := FromTree(es, e.Root, nil, 20<<20)
	require.NoError(t, err)
	paths := map[string]bool{}
	for _, f := range s.Files {
		paths[f.Path] = true
	}
	require.True(t, paths[".claude-plugin/plugin.json"])
	require.True(t, paths["skills/spec-feature/SKILL.md"])
	m, err := s.Manifest()
	require.NoError(t, err)
	t.Logf("plugin %s %s: sha256 %s, %d bytes, %d files, %d skills, in %s", m.Name, m.Version, s.SHA256, s.Size, len(s.Files), len(m.Skills), time.Since(start).Round(time.Millisecond))
}

// pb builds packfiles by hand for the hostile cases a git server would never send.
type pb struct {
	buf bytes.Buffer
	n   int
	zw  *zlib.Writer
}

func (p *pb) hdr(typ byte, size int) {
	c := typ<<4 | byte(size&15)
	size >>= 4
	for size > 0 {
		p.buf.WriteByte(c | 0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	p.buf.WriteByte(c)
}

func (p *pb) z(data []byte) {
	if p.zw == nil {
		p.zw = zlib.NewWriter(&p.buf)
	} else {
		p.zw.Reset(&p.buf)
	}
	_, _ = p.zw.Write(data)
	_ = p.zw.Close()
}

func (p *pb) obj(typ byte, data []byte) oid {
	p.hdr(typ, len(data))
	p.z(data)
	p.n++
	return hashObject(&object{typ: typ, data: data})
}

func (p *pb) refDelta(base oid, delta []byte) {
	p.hdr(objRefDelta, len(delta))
	p.buf.Write(base[:])
	p.z(delta)
	p.n++
}

func (p *pb) bytes() []byte {
	var h [12]byte
	copy(h[:], "PACK")
	binary.BigEndian.PutUint32(h[4:], 2)
	binary.BigEndian.PutUint32(h[8:], uint32(p.n))
	return append(h[:], p.buf.Bytes()...)
}

func treeEnt(mode, name string, id oid) []byte {
	return append([]byte(mode+" "+name+"\x00"), id[:]...)
}

// 009-AC9: trees naming one subtree twice per level stop at the entry cap, not at 2^depth.
func TestWalkTreeBomb(t *testing.T) {
	p := &pb{}
	cur := p.obj(objTree, treeEnt("100644", "f", p.obj(objBlob, []byte("x"))))
	for range 17 {
		cur = p.obj(objTree, append(treeEnt("40000", "a", cur), treeEnt("40000", "b", cur)...))
	}
	commit := p.obj(objCommit, fmt.Appendf(nil, "tree %s\n\nm\n", cur))
	objs, _, err := parsePack(t.Context(), bytes.NewReader(p.bytes()), 20<<20)
	require.NoError(t, err)
	_, err = walk(t.Context(), objs, commit.String(), 20<<20)
	require.ErrorIs(t, err, ErrTooBig)
	require.ErrorContains(t, err, "more than 100000 files and folders")

	// The same blob listed under many names counts its bytes each time.
	p = &pb{}
	blob := p.obj(objBlob, make([]byte, 600))
	var tree []byte
	for i := range 8 {
		tree = append(tree, treeEnt("100644", fmt.Sprintf("f%d", i), blob)...)
	}
	commit = p.obj(objCommit, fmt.Appendf(nil, "tree %s\n", p.obj(objTree, tree)))
	objs, _, err = parsePack(t.Context(), bytes.NewReader(p.bytes()), 1000)
	require.NoError(t, err)
	_, err = walk(t.Context(), objs, commit.String(), 1000)
	require.ErrorContains(t, err, "the repo once unpacked is bigger than 4000 bytes")
}

// 009-AC9: long names nested deep are refused at the first one, before the walk joins them.
func TestWalkLongNames(t *testing.T) {
	for _, nameLen := range []int{16 << 10, 200} {
		p := &pb{}
		blob := p.obj(objBlob, []byte("x"))
		var leaf []byte
		for i := range 300 {
			leaf = append(leaf, treeEnt("100644", fmt.Sprintf("f%d", i), blob)...)
		}
		cur := p.obj(objTree, leaf)
		name := strings.Repeat("a", nameLen)
		for range 60 {
			cur = p.obj(objTree, treeEnt("40000", name, cur))
		}
		commit := p.obj(objCommit, fmt.Appendf(nil, "tree %s\n\nm\n", cur))
		objs, _, err := parsePack(t.Context(), bytes.NewReader(p.bytes()), 20<<20)
		require.NoError(t, err)
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		_, err = walk(t.Context(), objs, commit.String(), 20<<20)
		runtime.ReadMemStats(&m1)
		require.ErrorContains(t, err, "a name or path is too long")
		require.Less(t, m1.TotalAlloc-m0.TotalAlloc, uint64(1<<20), "walk allocated %d bytes", m1.TotalAlloc-m0.TotalAlloc)
	}
}

// 009-AC9: objects each under the cap but together over inflateFactor times it are refused.
func TestPackUnpackedBudget(t *testing.T) {
	p := &pb{}
	for i := range 5 {
		p.obj(objBlob, append(make([]byte, 900), byte(i)))
	}
	_, _, err := parsePack(t.Context(), bytes.NewReader(p.bytes()), 1000)
	require.ErrorContains(t, err, "the repo once unpacked is bigger than 4000 bytes")
}

// REF_DELTAs listed before their bases resolve in one pass each, not one pass per delta.
func TestPackRefDeltaLinear(t *testing.T) {
	const n = 3000
	p := &pb{}
	datas := make([][]byte, n+1)
	ids := make([]oid, n+1)
	for i := range datas {
		datas[i] = binary.BigEndian.AppendUint64(nil, uint64(i))
		ids[i] = hashObject(&object{typ: objBlob, data: datas[i]})
	}
	for i := n; i >= 1; i-- {
		p.refDelta(ids[i-1], append([]byte{8, 8, 8}, datas[i]...))
	}
	p.obj(objBlob, datas[0])
	objs, st, err := parsePack(t.Context(), bytes.NewReader(p.bytes()), 20<<20)
	require.NoError(t, err)
	require.Len(t, objs, n+1)
	require.Equal(t, n, st.deltaApplies)
	require.LessOrEqual(t, st.pendingScans, 2*n)

	q := &pb{}
	q.refDelta(ids[0], []byte{8, 8, 1, 'x'})
	_, _, err = parsePack(t.Context(), bytes.NewReader(q.bytes()), 20<<20)
	require.ErrorContains(t, err, "delta base "+ids[0].String()+" missing")
}

// H3: a header claiming more objects than any plugin repo needs is refused before allocating.
func TestPackObjectCount(t *testing.T) {
	_, _, err := parsePack(t.Context(), strings.NewReader("PACK\x00\x00\x00\x02\xff\xff\xff\xff"), 20<<20)
	require.ErrorContains(t, err, "more than 100000 objects")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := &pb{}
	p.obj(objBlob, []byte("x"))
	_, _, err = parsePack(ctx, bytes.NewReader(p.bytes()), 20<<20)
	require.ErrorIs(t, err, context.Canceled)
}

// 009-AC8: Resolve and Fetch stop at their overall deadline, whatever the server does.
func TestFetchDeadline(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	c := testClient(srv, func(string) []netip.Addr { return []netip.Addr{loopback} })
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	old := fetchTimeout
	fetchTimeout = 200 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	u := "https://example.com:" + port + "/r"
	_, _, err := Resolve(t.Context(), c, u, "")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = Fetch(t.Context(), c, u, strings.Repeat("a", 40), 20<<20)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
