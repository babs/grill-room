package plugin

import (
	"errors"
	"testing"
)

// 009-AC7 009-AC20: the AI host fetches https without userinfo, ssh with a user name and no
// password, and git@host:path; never another transport, nor anything git or ssh could read as an
// option.
func TestCheckHostURL(t *testing.T) {
	for raw, want := range map[string]error{
		"https://github.com/o/r.git":          nil,
		"ssh://git@github.com/o/r.git":        nil,
		"ssh://github.com:2222/o/r":           nil,
		"git@github.com:o/r.git":              nil,
		"me.name@git.example.com:team/r":      nil,
		"https://u:p@github.com/o/r":          ErrCredentials,
		"https://u@github.com/o/r":            ErrCredentials,
		"ssh://git:secret@github.com/o/r":     ErrCredentials,
		"http://github.com/o/r":               ErrHostURL,
		"file:///srv/repo.git":                ErrHostURL,
		"ext::sh -c id":                       ErrHostURL,
		"git://github.com/o/r":                ErrHostURL,
		"/srv/repo.git":                       ErrHostURL,
		"-oProxyCommand=id@h:p":               ErrHostURL,
		"git@-oProxyCommand=id:p":             ErrHostURL,
		"git@github.com:-u":                   ErrHostURL,
		"git@github.com:":                     ErrHostURL,
		"ssh://-oProxyCommand=id@github.com/": ErrHostURL,
		"ssh://-github.com/o/r":               ErrHostURL,
		"https://github.com/":                 ErrHostURL,
		"https://github.com/o/r?x=1":          ErrHostURL,
		"https://github.com/o/r\n":            ErrHostURL,
		"":                                    ErrHostURL,
	} {
		if err := CheckHostURL(raw); !errors.Is(err, want) || (want == nil) != (err == nil) {
			t.Errorf("%q: %v, want %v", raw, err, want)
		}
	}
}

func TestRepoNameAndRef(t *testing.T) {
	for raw, scp := range map[string]bool{"git@github.com:o/r.git": true, "https://github.com/o/r": false, "-x@h:p": false, "h:p": false} {
		if IsSCP(raw) != scp {
			t.Errorf("IsSCP(%q) = %v", raw, !scp)
		}
	}
	for raw, want := range map[string]string{"git@github.com:o/rich.git": "rich", "ssh://git@h/o/r/": "r", "https://h/o/x.git": "x", "git@h:": "", "::": ""} {
		if got := RepoName(raw); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", raw, got, want)
		}
	}
	for ref, ok := range map[string]bool{"": true, "main": true, "v1.2": true, "refs/heads/x": true, "-x": false, "--upload-pack=x": false, "a..b": false, "a b": false} {
		if err := CheckRef(ref); (err == nil) != ok {
			t.Errorf("CheckRef(%q): %v", ref, err)
		}
	}
}
