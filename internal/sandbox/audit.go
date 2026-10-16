package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// Scan counts occurrences of secret in the container filesystem (docker export), in its volumes,
// and in the inspect output of the sandbox and its relay (env included). Never returns the secret.
func (s *Sandbox) Scan(ctx context.Context, secret []byte) (map[string]int, error) {
	if len(secret) == 0 {
		return nil, errors.New("empty secret")
	}
	fsTar, err := s.run(ctx, nil, "export", s.container())
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	vols, err := s.run(ctx, nil, "exec", s.container(), "tar", "-c", "-C", "/", home[1:], Workspace[1:])
	if err != nil {
		return nil, fmt.Errorf("volumes: %w", err)
	}
	inspect, err := s.run(ctx, nil, "inspect", s.container(), s.relay())
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}
	return map[string]int{
		"filesystem": bytes.Count(fsTar, secret),
		"volumes":    bytes.Count(vols, secret),
		"inspect":    bytes.Count(inspect, secret),
	}, nil
}

// Isolated returns nil when the sandbox reaches the two proxies and nothing else: Anthropic's hosts
// do not resolve, a public IP does not connect, and the egress proxy refuses Anthropic.
func (s *Sandbox) Isolated(ctx context.Context) error {
	dial := func(target string) error {
		_, err := s.run(ctx, nil, "exec", s.container(), "timeout", "5", "bash", "-c", "exec 3<>/dev/tcp/"+target)
		return err
	}
	if _, err := s.run(ctx, nil, "exec", s.container(), "getent", "hosts", "api.anthropic.com"); err == nil {
		return errors.New("api.anthropic.com resolves inside the sandbox")
	}
	for _, target := range []string{"api.anthropic.com/443", "platform.claude.com/443", "1.1.1.1/443"} {
		if dial(target) == nil {
			return fmt.Errorf("%s is reachable from the sandbox", target)
		}
	}
	if err := dial("proxy/" + relayPort); err != nil {
		return fmt.Errorf("the proxy is unreachable from the sandbox: %w", err)
	}
	// The sandbox's https_proxy sends curl through squid, which must answer the CONNECT with 403:
	// the token host too, or a refresh gets an answer and wipes the credentials (S16, 014-AC6).
	for _, host := range []string{"api.anthropic.com", "platform.claude.com"} {
		out, _ := s.run(ctx, nil, "exec", s.container(), "curl", "-s", "-o", "/dev/null", "-m", "10", "-w", "%{http_connect}", "https://"+host)
		if code := strings.TrimSpace(string(out)); code != "403" {
			return fmt.Errorf("the egress proxy answered %q to %s, want 403", code, host)
		}
	}
	return nil
}
