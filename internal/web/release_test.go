package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeDaemon stands in for a release binary: install.sh execs it, and it prints its argument.
const fakeDaemon = "#!/bin/sh\necho \"daemon started for $1\"\n"

// writeRelease lays out a release as `make release` does, every target holding body.
func writeRelease(t *testing.T, version, body string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("VERSION", version+"\n", 0o644)
	sum := sha256.Sum256([]byte(body))
	for _, target := range DaemonTargets {
		f := DaemonFile(target)
		write(f, body, 0o755)
		write(f+".sha256", hex.EncodeToString(sum[:])+"  "+f+"\n", 0o644)
	}
	return dir
}

func TestLoadRelease(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(dir string) error
		want   string
	}{
		{"complete", func(string) error { return nil }, ""},
		{"no VERSION", func(d string) error { return os.Remove(filepath.Join(d, "VERSION")) }, "VERSION"},
		{"VERSION not semver", func(d string) error { return os.WriteFile(filepath.Join(d, "VERSION"), []byte("1.2\n"), 0o644) }, "not a semver"},
		{"missing sha256 file", func(d string) error { return os.Remove(filepath.Join(d, DaemonFile("darwin-arm64")+".sha256")) }, "darwin-arm64.sha256"},
		{"sha256 file names another binary", func(d string) error {
			return os.WriteFile(filepath.Join(d, DaemonFile("linux-amd64")+".sha256"), []byte(strings.Repeat("a", 64)+"  other\n"), 0o644)
		}, "not a sha256sum line"},
		{"sha256 not hex", func(d string) error {
			return os.WriteFile(filepath.Join(d, DaemonFile("linux-amd64")+".sha256"), []byte(strings.Repeat("z", 64)+"  "+DaemonFile("linux-amd64")+"\n"), 0o644)
		}, "not a sha256sum line"},
		{"missing binary", func(d string) error { return os.Remove(filepath.Join(d, DaemonFile("linux-arm64"))) }, "linux-arm64"},
		{"bundle not a file", func(d string) error {
			if err := os.WriteFile(filepath.Join(d, DaemonFile("linux-amd64")+".sigstore.json"), []byte("{}"), 0o644); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(d, DaemonFile("darwin-arm64")+".sigstore.json"), 0o755)
		}, "darwin-arm64.sigstore.json is not a readable file"},
		{"missing windows binary", func(d string) error { return os.Remove(filepath.Join(d, DaemonFile("windows-arm64"))) }, "windows-arm64.exe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRelease(t, "v1.2.3", fakeDaemon)
			if err := tt.mutate(dir); err != nil {
				t.Fatal(err)
			}
			r, err := LoadRelease(dir)
			if tt.want == "" {
				sum := sha256.Sum256([]byte(fakeDaemon))
				if err != nil || r.Version != "v1.2.3" || r.SHA256["darwin-amd64"] != hex.EncodeToString(sum[:]) || len(r.SHA256) != 6 || len(r.Bundles) != 0 {
					t.Fatalf("LoadRelease = %+v, %v", r, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// 001-AC36 001-AC37: the server publishes its daemon release, the sha256 of each binary and the
// sandbox image, and serves the release files by exact name only.
func TestReleaseRoutes(t *testing.T) {
	const image = "registry.example/grill-room/sandbox:v1@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	opts := testOptions
	opts.Release, _ = LoadRelease(writeRelease(t, "v1.2.3", fakeDaemon))
	opts.SandboxImage = image
	e := newEnvOpts(t, opts)

	code, body := e.do(t, http.MethodGet, "/api/daemon/version", "", nil)
	v := decodeJSON[struct {
		Version      string            `json:"version"`
		SHA256       map[string]string `json:"sha256"`
		Bundles      map[string]string `json:"bundles"`
		SandboxImage string            `json:"sandbox_image"`
	}](t, body)
	if code != http.StatusOK || v.Version != "v1.2.3" || v.SandboxImage != image || v.SHA256["linux-arm64"] != opts.Release.SHA256["linux-arm64"] ||
		len(v.SHA256) != 6 || v.Bundles == nil || len(v.Bundles) != 0 {
		t.Fatalf("version: %d %s", code, body)
	}
	for _, tt := range []struct {
		path, want string
		code       int
	}{
		{"/download/grill-room-daemon-linux-amd64", fakeDaemon, http.StatusOK},
		{"/download/grill-room-daemon-darwin-arm64.sha256", opts.Release.SHA256["darwin-arm64"] + "  grill-room-daemon-darwin-arm64\n", http.StatusOK},
		{"/download/grill-room-daemon-windows-arm64.exe", fakeDaemon, http.StatusOK},
		{"/download/grill-room-daemon-windows-amd64.exe.sha256", opts.Release.SHA256["windows-amd64"] + "  grill-room-daemon-windows-amd64.exe\n", http.StatusOK},
		{"/download/VERSION", "", http.StatusNotFound},
		{"/download/grill-room-daemon-windows-amd64", "", http.StatusNotFound},
		{"/download/grill-room-daemon-freebsd-amd64", "", http.StatusNotFound},
		// Unsigned: no bundle to serve.
		{"/download/grill-room-daemon-linux-amd64.sigstore.json", "", http.StatusNotFound},
		{"/download/..%2fVERSION", "", http.StatusNotFound},
		{"/install.sh", "main \"$@\"\n", http.StatusOK},
	} {
		code, body := e.do(t, http.MethodGet, tt.path, "", nil)
		if code != tt.code || !strings.HasSuffix(string(body), tt.want) {
			t.Errorf("GET %s = %d %q, want %d ending %q", tt.path, code, body, tt.code, tt.want)
		}
	}
}

// 003-AC1: a signed release serves each binary's sigstore bundle beside it and lists the bundle
// URLs; a target released without a bundle lists none and serves none.
func TestReleaseServesBundles(t *testing.T) {
	dir := writeRelease(t, "v1.2.3", fakeDaemon)
	const bundle = `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`
	signed := map[string]bool{}
	for _, target := range DaemonTargets {
		if target == "darwin-amd64" {
			continue
		}
		signed[target] = true
		if err := os.WriteFile(filepath.Join(dir, DaemonFile(target)+".sigstore.json"), []byte(bundle), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Not a target of this release: never listed nor served.
	if err := os.WriteFile(filepath.Join(dir, "grill-room-daemon-freebsd-amd64.sigstore.json"), []byte(bundle), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := testOptions
	var err error
	if opts.Release, err = LoadRelease(dir); err != nil {
		t.Fatal(err)
	}
	// Written after the release was loaded: not part of what this server hands out.
	if err := os.WriteFile(filepath.Join(dir, DaemonFile("darwin-amd64")+".sigstore.json"), []byte(bundle), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEnvOpts(t, opts)

	code, body := e.do(t, http.MethodGet, "/api/daemon/version", "", nil)
	v := decodeJSON[struct {
		Bundles map[string]string `json:"bundles"`
	}](t, body)
	if code != http.StatusOK || len(v.Bundles) != len(signed) {
		t.Fatalf("version: %d %s", code, body)
	}
	for _, target := range DaemonTargets {
		url, listed := v.Bundles[target]
		if listed != signed[target] {
			t.Errorf("bundle of %s listed = %v, want %v", target, listed, signed[target])
		}
		path := "/download/" + DaemonFile(target) + ".sigstore.json"
		if listed && url != path {
			t.Errorf("bundle URL of %s = %q, want %q", target, url, path)
		}
		resp, got := e.get(t, path, "")
		switch {
		case signed[target] && (resp.StatusCode != http.StatusOK || string(got) != bundle || resp.Header.Get("Content-Type") != "application/json"):
			t.Errorf("GET %s = %d %q %q, want the bundle as JSON", path, resp.StatusCode, got, resp.Header.Get("Content-Type"))
		case !signed[target] && resp.StatusCode != http.StatusNotFound:
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
	if code, _ := e.do(t, http.MethodGet, "/download/grill-room-daemon-freebsd-amd64.sigstore.json", "", nil); code != http.StatusNotFound {
		t.Errorf("foreign bundle served: %d", code)
	}
}

func TestNoReleaseOffersNoDownload(t *testing.T) {
	e := newEnv(t)
	code, body := e.do(t, http.MethodGet, "/api/daemon/version", "", nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"version":""`) || !strings.Contains(string(body), `"sandbox_image":""`) ||
		!strings.Contains(string(body), `"bundles":{}`) {
		t.Fatalf("version: %d %s", code, body)
	}
	for _, p := range []string{"/install.sh", "/download/grill-room-daemon-linux-amd64"} {
		if code, _ := e.do(t, http.MethodGet, p, "", nil); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
}

// installShells are the shells install.sh must run under: `curl | sh` is dash on Debian and
// Ubuntu, bash elsewhere, busybox ash on Alpine.
func installShells(t *testing.T) [][]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh installs on Linux and macOS; it refuses MSYS and MinGW")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	var shells [][]string
	for _, sh := range [][]string{{"dash"}, {"bash", "--posix"}, {"busybox", "sh"}} {
		if _, err := exec.LookPath(sh[0]); err == nil {
			shells = append(shells, sh)
		}
	}
	if len(shells) < 2 {
		t.Skipf("need at least two of dash, bash, busybox; found %v", shells)
	}
	return shells
}

// runInstall pipes the served install.sh into shell, as the one-liner does.
func runInstall(t *testing.T, e *env, shell []string, home string, args ...string) (string, error) {
	t.Helper()
	code, script := e.do(t, http.MethodGet, "/install.sh", "", nil)
	if code != http.StatusOK {
		t.Fatalf("install.sh: %d", code)
	}
	cmd := exec.Command(shell[0], append(append(shell[1:], "-s", "--"), args...)...) //nolint:gosec,noctx // test shells
	cmd.Stdin = bytes.NewReader(script)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// 001-AC36: the one-liner installs the binary in ~/.local/bin after checking its sha256, and
// starts it with the room URL, under every shell `curl | sh` may land in.
func TestInstallScriptInstallsAndStarts(t *testing.T) {
	opts := testOptions
	opts.Release, _ = LoadRelease(writeRelease(t, "v1.2.3", fakeDaemon))
	e := newEnvOpts(t, opts)
	room := e.ts.URL + "/r/abc123"
	for _, sh := range installShells(t) {
		t.Run(sh[0], func(t *testing.T) {
			home := t.TempDir()
			out, err := runInstall(t, e, sh, home, room)
			if err != nil || !strings.Contains(out, "daemon started for "+room) {
				t.Fatalf("install: %v\n%s", err, out)
			}
			bin := filepath.Join(home, ".local", "bin", "grill-room-daemon")
			st, err := os.Stat(bin)
			if err != nil || st.Mode().Perm() != 0o755 {
				t.Fatalf("installed binary: %v %v", st, err)
			}
			if left, _ := filepath.Glob(filepath.Join(home, ".local", "bin", ".grill-room-daemon.*")); len(left) != 0 {
				t.Fatalf("temp files left: %v", left)
			}
		})
	}
}

// 001-AC36: a binary whose sha256 differs from the published one is refused and nothing is
// installed, and a bad room URL is a usage error.
func TestInstallScriptRefuses(t *testing.T) {
	dir := writeRelease(t, "v1.2.3", fakeDaemon)
	opts := testOptions
	opts.Release, _ = LoadRelease(dir)
	e := newEnvOpts(t, opts)
	// Tampered after the release was published: the .sha256 files no longer match.
	for _, target := range DaemonTargets {
		if err := os.WriteFile(filepath.Join(dir, DaemonFile(target)), []byte(fakeDaemon+"echo pwned\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, sh := range installShells(t) {
		for _, tt := range []struct {
			name string
			args []string
			exit int
			want string
		}{
			{"tampered binary", []string{e.ts.URL + "/r/abc123"}, 1, "sha256 mismatch"},
			{"no room URL", nil, 2, "usage:"},
			{"plain http to another host", []string{"http://rooms.example/r/abc123"}, 2, "usage:"},
			{"not a room URL", []string{e.ts.URL + "/join"}, 2, "usage:"},
			{"two arguments", []string{e.ts.URL + "/r/abc123", "extra"}, 2, "usage:"},
		} {
			t.Run(fmt.Sprintf("%s/%s", sh[0], tt.name), func(t *testing.T) {
				home := t.TempDir()
				out, err := runInstall(t, e, sh, home, tt.args...)
				ee, ok := err.(*exec.ExitError)
				if !ok || ee.ExitCode() != tt.exit || !strings.Contains(out, tt.want) || strings.Contains(out, "daemon started") {
					t.Fatalf("exit %v, want %d with %q:\n%s", err, tt.exit, tt.want, out)
				}
				if entries, _ := os.ReadDir(filepath.Join(home, ".local", "bin")); len(entries) != 0 {
					t.Fatalf("installed anyway: %v", entries)
				}
			})
		}
	}
}
