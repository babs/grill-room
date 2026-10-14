package plugin

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// ErrHostURL refuses a link the AI host would not fetch: only https, ssh and git's scp-like
// user@host:path form (009-AC20).
var ErrHostURL = errors.New("the AI host fetches https or ssh links only, as https://host/owner/repo, ssh://git@host/owner/repo or git@host:owner/repo")

// scpRe is git's scp-like ssh form, user@host:path. Neither part may start with a dash: git and
// ssh would read it as an option.
var scpRe = regexp.MustCompile(`^([A-Za-z0-9._][A-Za-z0-9._-]*)@([A-Za-z0-9][A-Za-z0-9.-]*):([^\x00-\x20:]*)$`)

// hostRe is a host name or IPv4 address; IPv6 literals are left out.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

// IsSCP reports git's scp-like ssh form, user@host:path.
func IsSCP(raw string) bool { return scpRe.MatchString(raw) }

// CheckHostURL validates a repo link the AI host's git fetches (009-AC20): https without any
// userinfo, ssh:// with a user name and no password, or user@host:path. file://, ext:: and the
// other transports git knows are refused, and so is anything git or ssh could take for an option.
// The error never quotes the link.
func CheckHostURL(raw string) error {
	if raw == "" || strings.HasPrefix(raw, "-") || strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return ErrHostURL
	}
	if m := scpRe.FindStringSubmatch(raw); m != nil {
		if m[3] == "" || strings.HasPrefix(m[3], "-") {
			return ErrHostURL
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || !hostRe.MatchString(u.Hostname()) {
		return ErrHostURL
	}
	switch u.Scheme {
	case "https":
		if u.User != nil {
			return ErrCredentials
		}
	case "ssh":
		if _, hasPassword := u.User.Password(); hasPassword {
			return ErrCredentials
		}
		if u.User != nil && strings.HasPrefix(u.User.Username(), "-") {
			return ErrHostURL
		}
	default:
		return ErrHostURL
	}
	if strings.Trim(u.Path, "/") == "" {
		return ErrHostURL
	}
	return nil
}

// RepoName is a repo link's last path element, its .git cut: the name Claude Code gives a plugin
// with no plugin.json at the repo's root. It reads the scp-like form too.
func RepoName(raw string) string {
	p := ""
	if m := scpRe.FindStringSubmatch(raw); m != nil {
		p = m[3]
	} else if u, err := url.Parse(raw); err == nil {
		p = u.Path
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	return strings.TrimSuffix(path.Base(p), ".git")
}

// CheckRef validates a ref as Resolve reads it: "" for the default branch, a branch or tag name,
// or a full commit hash. None may start with a dash, so git never reads one as an option.
func CheckRef(ref string) error {
	if ref == "" || hexRe.MatchString(ref) {
		return nil
	}
	if !refRe.MatchString(ref) || strings.Contains(ref, "..") {
		return fmt.Errorf("%q is not a valid branch, tag or commit", ref)
	}
	return nil
}
