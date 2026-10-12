package daemon

import (
	"testing"
)

// 003-AC11: the supervisor's roots follow each platform's conventions, from the environment it is
// given; GRILL_ROOM_DAEMON_STATE_DIR keeps everything in one directory, as before.
func TestResolvePaths(t *testing.T) {
	for _, tc := range []struct {
		name, goos, home string
		env              map[string]string
		want             Paths
		legacy           string
	}{
		{"linux defaults", "linux", "/home/al", nil,
			Paths{"/home/al/.config/grill-room", "/home/al/.local/state/grill-room", "/home/al/.cache/grill-room"}, "/home/al/.config/grill-room-daemon"},
		{"linux XDG", "linux", "/home/al", map[string]string{"XDG_CONFIG_HOME": "/x/conf", "XDG_STATE_HOME": "/x/state", "XDG_CACHE_HOME": "/x/cache"},
			Paths{"/x/conf/grill-room", "/x/state/grill-room", "/x/cache/grill-room"}, "/x/conf/grill-room-daemon"},
		// The XDG spec: a relative value is ignored.
		{"linux relative XDG", "linux", "/home/al", map[string]string{"XDG_CONFIG_HOME": "conf", "XDG_STATE_HOME": "./state"},
			Paths{"/home/al/.config/grill-room", "/home/al/.local/state/grill-room", "/home/al/.cache/grill-room"}, "/home/al/.config/grill-room-daemon"},
		// WSL is a plain Linux.
		{"wsl", "linux", "/home/al", map[string]string{"WSL_DISTRO_NAME": "Ubuntu"},
			Paths{"/home/al/.config/grill-room", "/home/al/.local/state/grill-room", "/home/al/.cache/grill-room"}, "/home/al/.config/grill-room-daemon"},
		{"darwin", "darwin", "/Users/al", map[string]string{"XDG_CONFIG_HOME": "/ignored"},
			Paths{"/Users/al/Library/Application Support/grill-room", "/Users/al/Library/Application Support/grill-room", "/Users/al/Library/Caches/grill-room"},
			"/Users/al/Library/Application Support/grill-room-daemon"},
		{"windows", "windows", `C:\Users\al`, map[string]string{"APPDATA": `C:\Users\al\AppData\Roaming`, "LOCALAPPDATA": `C:\Users\al\AppData\Local\`},
			Paths{`C:\Users\al\AppData\Roaming\grill-room`, `C:\Users\al\AppData\Local\grill-room`, `C:\Users\al\AppData\Local\grill-room`},
			`C:\Users\al\AppData\Roaming\grill-room-daemon`},
		// A redirected %APPDATA% keeps its UNC prefix.
		{"windows UNC", "windows", "", map[string]string{"APPDATA": `\\files\profiles\al\AppData\Roaming`, "LOCALAPPDATA": `C:\Users\al\AppData\Local`},
			Paths{`\\files\profiles\al\AppData\Roaming\grill-room`, `C:\Users\al\AppData\Local\grill-room`, `C:\Users\al\AppData\Local\grill-room`},
			`\\files\profiles\al\AppData\Roaming\grill-room-daemon`},
		{"override", "darwin", "/Users/al", map[string]string{"GRILL_ROOM_DAEMON_STATE_DIR": "rel/state"},
			Paths{"rel/state", "rel/state", "rel/state"}, "rel/state"},
		{"override windows", "windows", "", map[string]string{"GRILL_ROOM_DAEMON_STATE_DIR": `D:\gr`}, Paths{`D:\gr`, `D:\gr`, `D:\gr`}, `D:\gr`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, legacy, err := ResolvePaths(tc.goos, func(k string) string { return tc.env[k] }, tc.home)
			if err != nil || p != tc.want || legacy != tc.legacy {
				t.Fatalf("got %+v %q %v\nwant %+v %q", p, legacy, err, tc.want, tc.legacy)
			}
		})
	}
	for _, tc := range []struct {
		name, goos string
		env        map[string]string
	}{
		{"linux without home", "linux", map[string]string{"XDG_CONFIG_HOME": "/x/conf"}},
		{"darwin without home", "darwin", nil},
		{"windows without APPDATA", "windows", map[string]string{"LOCALAPPDATA": `C:\L`}},
		{"windows without LOCALAPPDATA", "windows", map[string]string{"APPDATA": `C:\R`}},
	} {
		if _, _, err := ResolvePaths(tc.goos, func(k string) string { return tc.env[k] }, ""); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}
	// With every XDG directory set, no home is needed.
	all := map[string]string{"XDG_CONFIG_HOME": "/c", "XDG_STATE_HOME": "/s", "XDG_CACHE_HOME": "/k"}
	if _, _, err := ResolvePaths("linux", func(k string) string { return all[k] }, ""); err != nil {
		t.Fatal(err)
	}
}

// Rooms on one server share an instance whatever the spelling of its URL; another scheme or port
// is another server.
func TestInstanceID(t *testing.T) {
	id := func(u string) string {
		t.Helper()
		got, err := InstanceID(u)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	a := id("https://rooms.example")
	if len(a) != 12 {
		t.Fatalf("id %q", a)
	}
	for _, same := range []string{"https://ROOMS.example", "HTTPS://rooms.example:443", "https://rooms.example/sub/path"} {
		if id(same) != a {
			t.Errorf("%s: another instance", same)
		}
	}
	for _, other := range []string{"http://rooms.example", "https://rooms.example:8443", "https://other.example"} {
		if id(other) == a {
			t.Errorf("%s: same instance", other)
		}
	}
	if id("http://127.0.0.1") != id("http://127.0.0.1:80") || id("http://[::1]:8080") == id("http://[::1]") {
		t.Error("default ports")
	}
	if _, err := InstanceID("not a url"); err == nil {
		t.Error("no host accepted")
	}
}
