package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseRoomURL(t *testing.T) {
	for _, tc := range []struct {
		in, base, id string
		ok           bool
	}{
		{"https://rooms.example/r/abc123", "https://rooms.example", "abc123", true},
		{" https://rooms.example/r/abc123/ ", "https://rooms.example", "abc123", true},
		{"https://rooms.example/r/abc123?pair=x#frag", "https://rooms.example", "abc123", true},
		{"https://rooms.example/prefix/r/abc123", "https://rooms.example/prefix", "abc123", true},
		{"https://rooms.example:8443/r/abc", "https://rooms.example:8443", "abc", true},
		{"http://127.0.0.1:7690/r/abc", "http://127.0.0.1:7690", "abc", true},
		{"http://localhost:7690/r/abc", "http://localhost:7690", "abc", true},
		{"http://rooms.example/r/abc", "", "", false},
		{"ftp://rooms.example/r/abc", "", "", false},
		{"https://rooms.example/join#token", "", "", false},
		{"https://rooms.example/r/ABC", "", "", false},
		{"https://rooms.example/r/a/b", "", "", false},
		{"rooms.example/r/abc", "", "", false},
		{"", "", "", false},
	} {
		r, err := ParseRoomURL(tc.in)
		if (err == nil) != tc.ok || r.Base != tc.base || r.ID != tc.id {
			t.Errorf("ParseRoomURL(%q) = %+v, %v", tc.in, r, err)
		}
	}
}

func TestTokens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	tk := NewTokens(dir)
	a, b := Room{Base: "https://x", ID: "a"}, Room{Base: "https://x", ID: "b"}
	if tok, err := tk.Get(a); err != nil || tok != "" {
		t.Fatalf("empty get = %q, %v", tok, err)
	}
	if err := tk.Set(a, "ta"); err != nil {
		t.Fatal(err)
	}
	if err := tk.Set(b, "tb"); err != nil {
		t.Fatal(err)
	}
	if err := tk.Set(a, ""); err != nil {
		t.Fatal(err)
	}
	if tok, _ := tk.Get(a); tok != "" {
		t.Fatalf("deleted token = %q", tok)
	}
	if tok, _ := tk.Get(b); tok != "tb" {
		t.Fatalf("b = %q", tok)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, "tokens.json"))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("tokens.json mode = %v, %v", st.Mode(), err)
		}
		if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("state dir mode = %v, %v", st.Mode(), err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tokens.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Get(a); err == nil {
		t.Fatal("corrupt file accepted")
	}
}

func TestRing(t *testing.T) {
	r := NewRing(3)
	_, _ = r.Write([]byte("a\n"))
	if got := r.Lines(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("lines = %q", got)
	}
	_, _ = r.Write([]byte("b\nc\n"))
	_, _ = r.Write([]byte("d\n"))
	if got := r.Lines(); len(got) != 3 || got[0] != "b" || got[2] != "d" {
		t.Fatalf("lines = %q", got)
	}
}

// A restarted worker must find its own sandbox again; two supervisors must never share one.
func TestSandboxName(t *testing.T) {
	a := Room{Base: "https://rooms.example", ID: "abc"}
	b := Room{Base: "https://rooms.example", ID: "abd"}
	n := SandboxName("/state/one", a)
	if len(n) != 12 || n != SandboxName("/state/one", a) {
		t.Fatalf("name %q not stable", n)
	}
	for _, other := range []string{SandboxName("/state/two", a), SandboxName("/state/one", b)} {
		if other == n {
			t.Fatalf("names collide: %q", n)
		}
	}
}
