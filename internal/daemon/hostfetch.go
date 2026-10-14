package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/plugin"
)

const (
	// fetchInflate bounds the fetched objects and the tree's files to this many times the
	// snapshot cap, as the server's fetch does: the commit's tree holds the whole repo.
	fetchInflate = 4
	// maxTreeEntries bounds the files of the commit's tree, whatever their sizes.
	maxTreeEntries = 100_000
	// fetchWatchEvery is how often the fetch's folder is measured while git runs.
	fetchWatchEvery = 250 * time.Millisecond
)

// hostFetchTimeout bounds a whole fetch through the host's git, as the server's (009-AC8); a var
// for tests.
var hostFetchTimeout = 60 * time.Second

// gitWaitDelay bounds the wait for git's output once it is killed: a process git started that
// outlives it, and keeps its pipes open, no longer holds the fetch.
const gitWaitDelay = 5 * time.Second

// errUnreachable is the room's words for any failure of the host's git (009-AC21).
var errUnreachable = errors.New("This AI host cannot reach the repo") //nolint:revive,staticcheck // a sentence the room shows

// gitConfig keeps the host's git config, where its credential helpers live, and overrides what a
// fetch of an untrusted repo must not do: other transports (file://, ext::, a URL rewritten to
// one), hooks, submodules, a template's hooks, an fsmonitor.
var gitConfig = []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always",
	"-c", "core.hooksPath=" + os.DevNull, "-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false",
	"-c", "credential.interactive=never", "-c", "init.templateDir=", "-c", "core.fsmonitor=false", "-c", "core.askPass="}

// gitEnv adds to the host's environment: no prompt anywhere (git's terminal, an askpass program,
// Git Credential Manager, ssh's askpass), https and ssh only, no LFS download. An empty
// GIT_ASKPASS stops git before core.askPass and SSH_ASKPASS.
var gitEnv = []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GCM_INTERACTIVE=never", "SSH_ASKPASS_REQUIRE=never",
	"GIT_ALLOW_PROTOCOL=https:ssh", "GIT_LFS_SKIP_SMUDGE=1"}

// gitError is a failure of the host's git; its stderr stays in the daemon's log.
type gitError struct {
	cmd    string
	err    error
	stderr string
}

func (e *gitError) Error() string { return "git " + e.cmd + ": " + e.err.Error() + ": " + e.stderr }
func (e *gitError) Unwrap() error { return e.err }

// gitCmd is the host's git in dir, outside every sandbox, with its own ssh keys, gh auth and
// credential helpers (009-AC20). The context's end kills it with what it started (detach).
func gitCmd(ctx context.Context, dir string, stdin io.Reader, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(gitConfig), args...)...) //nolint:gosec // fixed git arguments; the link and ref are checked by plugin.CheckHostURL and CheckRef
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv...)
	cmd.Stdin = stdin
	cmd.WaitDelay = gitWaitDelay
	detach(cmd)
	return cmd
}

// hostGit runs gitCmd and returns its output.
func hostGit(ctx context.Context, dir string, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := gitCmd(ctx, dir, stdin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, &gitError{cmd: args[0], err: err, stderr: truncateLine(stderr.Bytes())}
	}
	return out, nil
}

// dirSize is the bytes of the regular files under dir.
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// fetchCommit fetches ref of repo, at depth 1, into a bare repo in dir, and returns its commit and
// the ref's name ("" names the default branch). The folder is measured while git runs: past
// limit, git is stopped and the fetch is too big.
func fetchCommit(ctx context.Context, dir, repo, ref string, limit int64) (commit, refName string, err error) {
	if _, err := hostGit(ctx, dir, nil, "init", "-q", "--bare", "."); err != nil {
		return "", "", err
	}
	want := ref
	if want == "" {
		want = "HEAD"
	}
	fctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		tick := time.NewTicker(fetchWatchEvery)
		defer tick.Stop()
		for {
			select {
			case <-fctx.Done():
				return
			case <-tick.C:
				if dirSize(dir) > limit {
					cancel(plugin.ErrTooBig)
					return
				}
			}
		}
	}()
	_, err = hostGit(fctx, dir, nil, "fetch", "-q", "--depth", "1", "--no-tags", "--", repo, want)
	if cause := context.Cause(fctx); errors.Is(cause, plugin.ErrTooBig) || dirSize(dir) > limit {
		return "", "", fmt.Errorf("%w: the repo is over %d MB at that commit", plugin.ErrTooBig, limit>>20)
	}
	if err != nil {
		return "", "", err
	}
	out, err := hostGit(ctx, dir, nil, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", "", err
	}
	commit = strings.TrimSpace(string(out))
	refName = ref
	if refName == "" {
		refName = "HEAD"
		// The name the room shows; a server that does not say keeps "HEAD".
		if out, err := hostGit(ctx, dir, nil, "ls-remote", "--symref", "--", repo, "HEAD"); err == nil {
			line, _, _ := strings.Cut(string(out), "\n")
			if target, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
				refName, _, _ = strings.Cut(target, "\t")
			}
		}
	}
	return commit, refName, nil
}

// readTree lists the fetched commit's files as the server's own fetch does, symlinks and
// submodules included, with git's raw bytes: no checkout, so no filter, hook or attribute runs.
func readTree(ctx context.Context, dir string, entries []string, limit int64) ([]plugin.Entry, error) {
	out, err := hostGit(ctx, dir, nil, "ls-tree", "-r", "-z", "-l", "--full-tree", "FETCH_HEAD")
	if err != nil {
		return nil, err
	}
	var es []plugin.Entry
	var blobs []string
	var total int64
	for rec := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 {
			return nil, fmt.Errorf("git ls-tree: unexpected line %q", rec)
		}
		if len(es) == maxTreeEntries {
			return nil, fmt.Errorf("%w: more than %d files in the repo", plugin.ErrTooBig, maxTreeEntries)
		}
		var k plugin.Kind
		oid := f[2]
		switch f[0] {
		case "100644":
			k = plugin.Regular
		case "100755":
			k = plugin.Executable
		case "120000":
			k = plugin.Symlink
		case "160000":
			es, blobs = append(es, plugin.Entry{Path: p, Kind: plugin.Submodule}), append(blobs, "")
			continue
		default:
			continue
		}
		size, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("git ls-tree: size %q", f[3])
		}
		if total += size; total > limit {
			return nil, fmt.Errorf("%w: the repo is over %d MB at that commit", plugin.ErrTooBig, limit>>20)
		}
		es, blobs = append(es, plugin.Entry{Path: p, Kind: k}), append(blobs, oid)
	}
	// Only the files the request needs are read: the marketplace, then the picked plugins' roots,
	// so memory holds what the snapshots take, not the repo.
	mi := slices.IndexFunc(es, func(e plugin.Entry) bool { return e.Path == plugin.MarketplacePath })
	if mi >= 0 {
		if err := readBlobs(ctx, dir, es, blobs, func(i int) bool { return i == mi }, limit); err != nil {
			return nil, err
		}
	}
	m, err := plugin.ReadMarketplace(es)
	if err != nil {
		return nil, err
	}
	prefixes := []string{""}
	if m != nil {
		prefixes = nil
		for _, e := range m.Plugins {
			if e.Pickable && slices.Contains(entries, e.Name) {
				prefixes = append(prefixes, rootPrefix(e.Root))
			}
		}
	}
	want := func(i int) bool {
		return i != mi && slices.ContainsFunc(prefixes, func(pre string) bool { return strings.HasPrefix(es[i].Path, pre) })
	}
	if err := readBlobs(ctx, dir, es, blobs, want, limit); err != nil {
		return nil, err
	}
	return es, nil
}

// rootPrefix is the path prefix of a marketplace entry's files, "" for the repo's root; FromTree
// checks the root itself.
func rootPrefix(root string) string {
	r := path.Clean(strings.TrimPrefix(root, "./"))
	if r == "." || r == "" {
		return ""
	}
	return r + "/"
}

// readBlobs fills in the entries of es that want picks, but submodules, with the blobs git
// cat-file --batch streams; blobs holds each entry's object id. It reads at most limit bytes.
func readBlobs(ctx context.Context, dir string, es []plugin.Entry, blobs []string, want func(int) bool, limit int64) error {
	var pick []int
	var ids []string
	for i := range es {
		if es[i].Kind != plugin.Submodule && want(i) {
			pick, ids = append(pick, i), append(ids, blobs[i])
		}
	}
	if len(pick) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCmd(ctx, dir, strings.NewReader(strings.Join(ids, "\n")+"\n"), "cat-file", "--batch")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git cat-file: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return &gitError{cmd: "cat-file", err: err}
	}
	readErr := func() error {
		br := bufio.NewReader(pipe)
		left := limit
		for i, j := range pick {
			head, err := br.ReadString('\n')
			f := strings.Fields(head)
			if err != nil || len(f) != 3 || f[0] != ids[i] || f[1] != "blob" {
				return fmt.Errorf("git cat-file: unexpected header %q", head)
			}
			n, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil || n < 0 {
				return fmt.Errorf("git cat-file: size %q", f[2])
			}
			if left -= n; left < 0 {
				return fmt.Errorf("%w: the repo is over %d MB at that commit", plugin.ErrTooBig, limit>>20)
			}
			es[j].Data = make([]byte, n)
			if _, err := io.ReadFull(br, es[j].Data); err != nil {
				return fmt.Errorf("git cat-file: %w", err)
			}
			if _, err := br.Discard(1); err != nil {
				return fmt.Errorf("git cat-file: %w", err)
			}
		}
		return nil
	}()
	if readErr != nil {
		cancel()
	}
	if err := cmd.Wait(); err != nil && readErr == nil {
		return &gitError{cmd: "cat-file", err: err, stderr: truncateLine(stderr.Bytes())}
	}
	return readErr
}

// requestError is a fetch failure the request itself caused, told to the room as it is: a ref git
// would not take, a marketplace without plugin names.
type requestError struct{ msg string }

func (e *requestError) Error() string { return e.msg }

func requestErrorf(format string, args ...any) error {
	return &requestError{msg: fmt.Sprintf(format, args...)}
}

// pickedSnapshot is one plugin of a fetched repo, by marketplace entry, "" for the repo itself.
type pickedSnapshot struct {
	entry string
	snap  *plugin.Snapshot
}

// snapshotsOf builds the plugins the request asks for out of the fetched tree, under the same rules
// as the server's own fetch (009-AC6, 009-AC9): the marketplace's entries named, or the repo as one
// plugin when it has no marketplace.
func snapshotsOf(tree []plugin.Entry, repo string, entries []string, maxBytes int64) ([]pickedSnapshot, error) {
	m, err := plugin.ReadMarketplace(tree)
	if err != nil {
		return nil, err
	}
	if m == nil {
		if len(entries) > 0 {
			return nil, requestErrorf("the repo has no marketplace: ask again without plugin names")
		}
		name, _ := json.Marshal(map[string]string{"name": plugin.RepoName(repo)}) // a string: cannot fail
		s, err := plugin.FromTree(tree, "", name, maxBytes)
		if err != nil {
			return nil, err
		}
		return []pickedSnapshot{{snap: s}}, nil
	}
	if len(entries) == 0 {
		var names []string
		for _, e := range m.Plugins {
			if e.Pickable {
				names = append(names, e.Name)
			}
		}
		return nil, requestErrorf("the repo is the marketplace %s: ask again naming the plugins to add, among %s", m.Name, strings.Join(names, ", "))
	}
	out := make([]pickedSnapshot, 0, len(entries))
	for _, name := range entries {
		var e *plugin.MarketEntry
		for i := range m.Plugins {
			if m.Plugins[i].Name == name {
				e = &m.Plugins[i]
			}
		}
		switch {
		case e == nil:
			return nil, requestErrorf("the marketplace lists no plugin %q", name)
		case !e.Pickable:
			return nil, requestErrorf("the plugin %s cannot be added: %s", name, e.Reason)
		}
		s, err := plugin.FromTree(tree, e.Root, e.PluginJSON(), maxBytes)
		if err != nil {
			return nil, fmt.Errorf("the plugin %s: %w", name, err)
		}
		out = append(out, pickedSnapshot{entry: name, snap: s})
	}
	return out, nil
}

// hostFetchRepo fetches what request f asks for with the host's git in a folder of its own under
// dir, removed after: the commit, the ref's name and the plugins' snapshots.
func hostFetchRepo(ctx context.Context, dir string, f HostFetch, maxBytes int64) (commit, refName string, snaps []pickedSnapshot, err error) {
	if err := plugin.CheckHostURL(f.URL); err != nil {
		return "", "", nil, err
	}
	if err := plugin.CheckRef(f.Ref); err != nil {
		return "", "", nil, &requestError{msg: err.Error()}
	}
	if maxBytes <= 0 || maxBytes > maxPluginBytes {
		maxBytes = maxPluginBytes
	}
	ctx, cancel := context.WithTimeout(ctx, hostFetchTimeout)
	defer cancel()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", nil, fmt.Errorf("fetch folder: %w", err)
	}
	tmp, err := os.MkdirTemp(dir, "fetch-*")
	if err != nil {
		return "", "", nil, fmt.Errorf("fetch folder: %w", err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // a leftover goes when the worker starts again (cmd/grill-room-daemon)
	limit := fetchInflate * maxBytes
	if commit, refName, err = fetchCommit(ctx, tmp, f.URL, f.Ref, limit); err != nil {
		return "", "", nil, err
	}
	tree, err := readTree(ctx, tmp, f.Entries, limit)
	if err != nil {
		return "", "", nil, err
	}
	snaps, err = snapshotsOf(tree, f.URL, f.Entries, maxBytes)
	return commit, refName, snaps, err
}

// roomReason is what the room reads of a failed fetch: git's failures are the host's access to the
// repo; what the repo or the request holds is said as it is; any other failure on this machine,
// a disk or a folder, stays in its log.
func roomReason(err error) string {
	var ge *gitError
	var re *requestError
	switch {
	case errors.As(err, &ge):
		return errUnreachable.Error()
	case errors.As(err, &re), errors.Is(err, plugin.ErrTooBig), errors.Is(err, plugin.ErrInvalid), errors.Is(err, plugin.ErrHostURL),
		errors.Is(err, plugin.ErrCredentials):
		return err.Error()
	}
	return "the AI host's computer failed to fetch it: the reason is in its log"
}

// fetchForRoom runs request f and posts its plugins to the room, or why it could not (009-AC20,
// 009-AC21). The room checks the snapshots again. Whatever git says stays in this log: the room
// reads "This AI host cannot reach the repo".
func (r *roomRun) fetchForRoom(ctx context.Context, tok string, f HostFetch, maxBytes int64) {
	log := r.d.o.Log.With(zap.String("room", r.room.String()), zap.Int64("fetch", f.ID))
	commit, refName, snaps, err := hostFetchRepo(ctx, r.d.o.FetchDir, f, maxBytes)
	if err != nil {
		log.Warn("plugin_fetch_failed", zap.Error(err))
		why := roomReason(err)
		if perr := r.postFetchFailure(ctx, tok, f.ID, fetchFailure{Error: why}); perr != nil {
			log.Warn("plugin_fetch_report_failed", zap.Error(perr))
		}
		return
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("commit", commit) // into memory: cannot fail
	_ = mw.WriteField("ref_name", refName)
	for _, s := range snaps {
		name := s.entry
		if name == "" {
			name = "."
		}
		w, _ := mw.CreateFormFile("snapshot", name)
		_, _ = w.Write(s.snap.Tar)
	}
	_ = mw.Close()
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodPost, r.fetchURL(f.ID), &body)
	if err != nil {
		log.Warn("plugin_fetch_upload_failed", zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := r.d.o.HTTP.Do(req)
	if err == nil {
		if resp.StatusCode/100 != 2 {
			err = apiError(resp)
		}
		_ = resp.Body.Close()
	}
	if err != nil {
		log.Warn("plugin_fetch_upload_failed", zap.Error(err))
		return
	}
	log.Info("plugin_fetched", zap.String("commit", commit), zap.Int("plugins", len(snaps)))
}
