package daemon

import (
	"errors"
	"os"
	"slices"
	"testing"
)

func fakePlatform(goos string, env map[string]string, osrelease string, have ...string) Platform {
	return Platform{
		GOOS:   goos,
		Getenv: func(k string) string { return env[k] },
		ReadFile: func(string) ([]byte, error) {
			if osrelease == "" {
				return nil, os.ErrNotExist
			}
			return []byte(osrelease), nil
		},
		LookPath: func(name string) (string, error) {
			if slices.Contains(have, name) {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
	}
}

// 001-AC22 001-AC23: the admin UI opens in the default browser on Linux, macOS and WSL.
func TestOpenCommand(t *testing.T) {
	const u = "http://127.0.0.1:1234/s/code"
	for _, tc := range []struct {
		name string
		p    Platform
		want []string
		wsl  bool
	}{
		{"linux", fakePlatform("linux", nil, "6.8.0-generic", "xdg-open"), []string{"xdg-open", u}, false},
		{"linux without xdg-open", fakePlatform("linux", nil, "6.8.0-generic"), nil, false},
		{"darwin", fakePlatform("darwin", nil, ""), []string{"open", u}, false},
		{"wsl by env, wslview", fakePlatform("linux", map[string]string{"WSL_DISTRO_NAME": "Ubuntu"}, "", "wslview", "xdg-open"), []string{"wslview", u}, true},
		{"wsl by kernel, explorer", fakePlatform("linux", nil, "5.15.167.4-microsoft-standard-WSL2", "xdg-open"), []string{"explorer.exe", u}, true},
		{"wsl1 kernel", fakePlatform("linux", nil, "4.4.0-19041-Microsoft"), []string{"explorer.exe", u}, true},
		{"windows native", fakePlatform("windows", nil, ""), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.WSL(); got != tc.wsl {
				t.Fatalf("WSL() = %v", got)
			}
			got, err := tc.p.OpenCommand(u)
			if tc.want == nil {
				if !errors.Is(err, ErrNoOpener) {
					t.Fatalf("got %v, %v; want ErrNoOpener", got, err)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestOpenURLStarts(t *testing.T) {
	p := fakePlatform("darwin", nil, "")
	var started []string
	p.Start = func(name string, args ...string) error { started = append([]string{name}, args...); return nil }
	if err := p.OpenURL("http://x"); err != nil || !slices.Equal(started, []string{"open", "http://x"}) {
		t.Fatalf("started %v, %v", started, err)
	}
}
