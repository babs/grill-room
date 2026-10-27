package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/mail"
	"github.com/babs/grill-room/internal/pgtest"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/store/storetest"
	"github.com/babs/grill-room/internal/web"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestConfigValidate(t *testing.T) {
	ok := Config{Addr: "a:1", MetricsAddr: "a:2", DatabaseURL: "postgres://u@db/x", LogLevel: "info", LogFormat: "auto",
		MaxUploadMB: 20, MaxRoomMB: 200, MaxAttachmentMB: 5, MaxImportMB: 1024, Pandoc: "pandoc", PandocTimeout: time.Minute, TurnTimeout: 15 * time.Minute, MagicLinkTTL: 15 * time.Minute,
		AuditRetention: 365 * 24 * time.Hour, ClientRetention: 720 * time.Hour, NotesWatchDelay: 20 * time.Second,
		VoiceParagraphMaxChars: 1200, VoiceParagraphClose: 20 * time.Second, AnonymousRoomsPerHour: 3}
	cases := map[string]func(*Config){
		"zero upload limit":                func(c *Config) { c.MaxUploadMB = 0 },
		"room below upload limit":          func(c *Config) { c.MaxRoomMB = 10 },
		"room limit overflows":             func(c *Config) { c.MaxRoomMB = 1 << 21 },
		"zero attachment limit":            func(c *Config) { c.MaxAttachmentMB = 0 },
		"attachment over room":             func(c *Config) { c.MaxAttachmentMB = 201 },
		"import below room":                func(c *Config) { c.MaxImportMB = 199 },
		"import overflows":                 func(c *Config) { c.MaxImportMB = 1<<20 + 1 },
		"zero pandoc timeout":              func(c *Config) { c.PandocTimeout = 0 },
		"turn timeout too short":           func(c *Config) { c.TurnTimeout = time.Second },
		"turn timeout too long":            func(c *Config) { c.TurnTimeout = 25 * time.Hour },
		"same addr":                        func(c *Config) { c.MetricsAddr = c.Addr },
		"empty database url":               func(c *Config) { c.DatabaseURL = "" },
		"bad level":                        func(c *Config) { c.LogLevel = "loud" },
		"bad format":                       func(c *Config) { c.LogFormat = "xml" },
		"sandbox image by tag":             func(c *Config) { c.SandboxImage = "registry.example/grill-room/sandbox:v1" },
		"sandbox image as a flag":          func(c *Config) { c.SandboxImage = "--privileged@sha256:" + strings.Repeat("a", 64) },
		"smtp without from":                func(c *Config) { c.SMTP.Addr = "mail.example:25" },
		"smtp from without addr":           func(c *Config) { c.SMTP.From = "noreply@example.com" },
		"smtp user, no password":           func(c *Config) { c.SMTP = mail.SMTP{Addr: "mail.example:25", From: "a@example.com", Username: "u"} },
		"magic link ttl too long":          func(c *Config) { c.MagicLinkTTL = 2 * time.Hour },
		"magic link ttl zero":              func(c *Config) { c.MagicLinkTTL = 0 },
		"magic link ttl not whole seconds": func(c *Config) { c.MagicLinkTTL = 1500 * time.Millisecond },
		"audit retention short":            func(c *Config) { c.AuditRetention = 23 * time.Hour },
		"client retention short":           func(c *Config) { c.ClientRetention = 23 * time.Hour },
		// 009-AC32: the notes watcher's delay.
		"notes delay too short": func(c *Config) { c.NotesWatchDelay = 500 * time.Millisecond },
		"notes delay too long":  func(c *Config) { c.NotesWatchDelay = 2 * time.Hour },
		// 018-AC4: the paragraph settings.
		"paragraph gap negative":    func(c *Config) { c.VoiceParagraphGap = -time.Second },
		"paragraph max chars zero":  func(c *Config) { c.VoiceParagraphMaxChars = 0 },
		"paragraph max chars above": func(c *Config) { c.VoiceParagraphMaxChars = 4001 },
		// 018-AC7: a paragraph never counts as closed before a line could still join it.
		"paragraph close zero":          func(c *Config) { c.VoiceParagraphClose = 0 },
		"paragraph close negative":      func(c *Config) { c.VoiceParagraphClose = -time.Second },
		"paragraph close below the gap": func(c *Config) { c.VoiceParagraphGap, c.VoiceParagraphClose = 3*time.Second, 2*time.Second },
		// 021-AC1: Validate checks the provider settings.
		"oidc over plain http": func(c *Config) {
			c.PublicURL, c.OIDC = "http://rooms.example", web.OIDCConfig{Issuer: "https://idp.example", ClientID: "c", ClientSecret: "s"}
		},
		// 019-AC4: the per-IP hourly limit.
		"anonymous rooms per hour zero":  func(c *Config) { c.AnonymousRoomsPerHour = 0 },
		"anonymous rooms per hour above": func(c *Config) { c.AnonymousRoomsPerHour = 1001 },
		"sandbox digest too short": func(c *Config) {
			c.SandboxImage = "registry.example/grill-room/sandbox@sha256:" + strings.Repeat("a", 63)
		},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	// 018-AC4: 0 turns appending off; a paragraph may be as long as a chat line.
	edges := ok
	edges.VoiceParagraphGap, edges.VoiceParagraphMaxChars = 0, 4000
	if err := edges.Validate(); err != nil {
		t.Fatalf("paragraph gap 0, max chars 4000: %v", err)
	}
	// 018-AC7: the close delay may equal the gap; with paragraphs off it only needs to be above 0.
	edges.VoiceParagraphGap, edges.VoiceParagraphClose = 3*time.Second, 3*time.Second
	if err := edges.Validate(); err != nil {
		t.Fatalf("paragraph close equal to the gap: %v", err)
	}
	edges.VoiceParagraphGap, edges.VoiceParagraphClose = 0, time.Millisecond
	if err := edges.Validate(); err != nil {
		t.Fatalf("paragraph gap 0, close 1ms: %v", err)
	}
	// 019-AC4: 1 and 1000 are the bounds.
	edges.AnonymousRoomsPerHour = 1
	if err := edges.Validate(); err != nil {
		t.Fatalf("anonymous rooms per hour 1: %v", err)
	}
	edges.AnonymousRoomsPerHour = 1000
	if err := edges.Validate(); err != nil {
		t.Fatalf("anonymous rooms per hour 1000: %v", err)
	}
	pinned := ok
	pinned.SandboxImage = "registry.example:5000/grill-room/sandbox:v1@sha256:" + strings.Repeat("a", 64)
	if err := pinned.Validate(); err != nil {
		t.Fatalf("pinned sandbox image: %v", err)
	}
	withMail := ok
	withMail.SMTP = mail.SMTP{Addr: "mail.example:587", From: "grill-room <noreply@example.com>", Username: "u", Password: "p"}
	if err := withMail.Validate(); err != nil || withMail.sendMail() == nil || ok.sendMail() != nil {
		t.Fatalf("smtp config: %v", err)
	}
	for name, mutate := range cases {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadConfigRejectsBadNumbers(t *testing.T) {
	t.Setenv("GRILL_ROOM_DATABASE_URL", "postgres://u@db/x")
	for key, val := range map[string]string{"GRILL_ROOM_MAX_UPLOAD_MB": "20MB", "GRILL_ROOM_MAX_ROOM_MB": "x", "GRILL_ROOM_PANDOC_TIMEOUT": "60", "GRILL_ROOM_MAX_ATTACHMENT_MB": "5MB", "GRILL_ROOM_TURN_TIMEOUT": "15", "GRILL_ROOM_MAGIC_LINK_TTL": "15", "GRILL_ROOM_AUDIT_RETENTION": "365", "GRILL_ROOM_CLIENT_RETENTION": "30", "GRILL_ROOM_TRUSTED_PROXIES": "10.0.0.0/33", "GRILL_ROOM_NOTES_WATCH_DELAY": "20", "GRILL_ROOM_VOICE_PARAGRAPH_GAP": "2.5", "GRILL_ROOM_VOICE_PARAGRAPH_MAX_CHARS": "1.2k", "GRILL_ROOM_VOICE_PARAGRAPH_CLOSE": "20",
		// 019-AC1, 019-AC4: a bad flag or limit refuses the start.
		"GRILL_ROOM_ANONYMOUS_ROOMS": "yes", "GRILL_ROOM_ANONYMOUS_ROOMS_PER_HOUR": "3.5"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, val)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("%s=%s accepted", key, val)
			}
		})
	}
	c, err := loadConfig()
	if err != nil || c.MaxUploadMB != 20 || c.MaxRoomMB != 200 || c.MaxAttachmentMB != 5 || c.TurnTimeout != 15*time.Minute || c.MagicLinkTTL != 15*time.Minute || c.AuditRetention != 8760*time.Hour || c.ClientRetention != 720*time.Hour || c.TrustedProxies != nil || c.NotesWatchDelay != 20*time.Second || c.VoiceParagraphGap != 5*time.Second || c.VoiceParagraphMaxChars != 1200 || c.VoiceParagraphClose != 20*time.Second || c.Validate() != nil {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	// 019-AC1, 019-AC4: off by default, 3 an hour; the bounds are checked at start.
	if c.AnonymousRooms || c.AnonymousRoomsPerHour != 3 {
		t.Fatalf("anonymous defaults: %v %d", c.AnonymousRooms, c.AnonymousRoomsPerHour)
	}
	for val, ok := range map[string]bool{"0": false, "1001": false, "1": true, "1000": true} {
		t.Setenv("GRILL_ROOM_ANONYMOUS_ROOMS_PER_HOUR", val)
		if c, err := loadConfig(); err != nil || (c.Validate() == nil) != ok {
			t.Errorf("GRILL_ROOM_ANONYMOUS_ROOMS_PER_HOUR=%s: load %v, valid %v", val, err, c.Validate())
		}
	}
	t.Setenv("GRILL_ROOM_ANONYMOUS_ROOMS", "true")
	if c, err := loadConfig(); err != nil || !c.AnonymousRooms {
		t.Fatalf("GRILL_ROOM_ANONYMOUS_ROOMS=true: %v %v", c.AnonymousRooms, err)
	}
}

// 019-AC4: anonymous rooms without a trusted proxy warn that clients behind one share the limit.
func TestWarnAnonRooms(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
		want int
	}{
		{"on, no proxy", Config{AnonymousRooms: true}, 1},
		{"on, trusted proxy", Config{AnonymousRooms: true, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}, 0},
		{"off", Config{}, 0},
	} {
		core, logs := observer.New(zapcore.WarnLevel)
		warnAnonRooms(c.cfg, zap.New(core))
		if n := logs.FilterMessage("anon_rooms_untrusted_peer").Len(); n != c.want {
			t.Errorf("%s: %d warnings, want %d", c.name, n, c.want)
		}
	}
}

// 012-AC5: an admin token that looks like an API token is refused at start.
func TestAdminTokenNotAPIToken(t *testing.T) {
	t.Setenv("GRILL_ROOM_ADMIN_TOKEN", "grt_x")
	t.Setenv("GRILL_ROOM_DATABASE_URL", "postgres://u@db/x")
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "grt_") {
		t.Fatalf("validate: %v", err)
	}
}

func TestAdminTokenHash(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()

	hash, generated, err := adminTokenHash(ctx, st, "")
	if err != nil || generated == "" || hash != web.HashToken(generated) {
		t.Fatalf("first start: %q %q %v", hash, generated, err)
	}
	stored, _ := st.Setting(ctx, adminTokenSetting)
	if stored != hash {
		t.Fatal("hash not persisted")
	}
	// Second start: same hash, nothing to show.
	hash2, generated2, err := adminTokenHash(ctx, st, "")
	if err != nil || hash2 != hash || generated2 != "" {
		t.Fatalf("restart: %q %q %v", hash2, generated2, err)
	}
	// The environment wins and does not touch the stored hash.
	hash3, generated3, err := adminTokenHash(ctx, st, "pinned")
	if err != nil || hash3 != web.HashToken("pinned") || generated3 != "" {
		t.Fatalf("env: %q %q %v", hash3, generated3, err)
	}
	if stored, _ := st.Setting(ctx, adminTokenSetting); stored != hash {
		t.Fatal("env token overwrote the stored hash")
	}
}

// 017 Q11: pods booting together on an empty database keep one admin token hash, and only the pod
// whose token was stored shows it.
func TestAdminTokenHashConcurrentBoots(t *testing.T) {
	dsn := storetest.DSN(t)
	type result struct {
		hash, generated string
		err             error
	}
	results := make(chan result, 2)
	var stores []*store.Store
	for range 2 {
		st, err := store.Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		stores = append(stores, st)
	}
	for _, st := range stores {
		go func() {
			hash, generated, err := adminTokenHash(t.Context(), st, "")
			results <- result{hash, generated, err}
		}()
	}
	var shown []string
	hashes := map[string]bool{}
	for range stores {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		hashes[r.hash] = true
		if r.generated != "" {
			shown = append(shown, r.generated)
		}
	}
	stored, err := stores[0].Setting(t.Context(), adminTokenSetting)
	if err != nil || len(hashes) != 1 || !hashes[stored] || len(shown) != 1 || web.HashToken(shown[0]) != stored {
		t.Fatalf("stored %q %v, hashes %v, shown %d", stored, err, hashes, len(shown))
	}
}

func TestValidateVoice(t *testing.T) {
	ok := Config{LiveKitURL: "wss://livekit.example", LiveKitAPIURL: "http://livekit:7880", LiveKitAPIKey: "k",
		LiveKitAPISecret: "0123456789abcdef0123456789abcdef", STTToken: "s"}
	if err := ok.validateVoice(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := (Config{}).validateVoice(); err != nil {
		t.Fatalf("voice off: %v", err)
	}
	cases := map[string]func(*Config){
		"stt without livekit": func(c *Config) { *c = Config{STTToken: "s"} },
		"http signalling":     func(c *Config) { c.LiveKitURL = "https://livekit.example" },
		"no host":             func(c *Config) { c.LiveKitURL = "wss://" },
		"ws api url":          func(c *Config) { c.LiveKitAPIURL = "ws://livekit:7880" },
		"no key":              func(c *Config) { c.LiveKitAPIKey = "" },
		"short secret":        func(c *Config) { c.LiveKitAPISecret = "short" },
	}
	for name, mutate := range cases {
		c := ok
		mutate(&c)
		if err := c.validateVoice(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	v := ok.voice()
	if v == nil || v.STTTokenHash != web.HashToken("s") || v.API.URL != "http://livekit:7880" {
		t.Fatalf("voice %+v", v)
	}
	if (Config{}).voice() != nil {
		t.Fatal("voice on without a URL")
	}
	t.Setenv("GRILL_ROOM_LIVEKIT_URL", "wss://livekit.example")
	if c, _ := loadConfig(); c.LiveKitAPIURL != "https://livekit.example" {
		t.Fatalf("default API URL %q", c.LiveKitAPIURL)
	}
}

// 005-AC24: GRILL_ROOM_PUSH_HOSTS replaces the push services subscriptions may name; unset keeps
// the default.
func TestPushHosts(t *testing.T) {
	t.Setenv("GRILL_ROOM_PUSH_HOSTS", " Push.Example.org, ,127.0.0.1 ")
	if c, err := loadConfig(); err != nil || !slices.Equal(c.PushHosts, []string{"push.example.org", "127.0.0.1"}) {
		t.Fatalf("hosts %q %v", c.PushHosts, err)
	}
	t.Setenv("GRILL_ROOM_PUSH_HOSTS", "")
	if c, _ := loadConfig(); c.PushHosts != nil {
		t.Fatalf("unset: %q", c.PushHosts)
	}
}

// 002-AC38: workspace fallbacks are counted on /metrics, one series per reason.
func TestRegistryCountsWorkspaceFallbacks(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := bus.New(st, web.NewToken())
	mfs, err := newRegistry(b, web.New(b, "", "http://x", web.Options{}, zap.NewNop())).Gather()
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, mf := range mfs {
		if mf.GetName() != "grill_room_workspace_fallbacks_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			reasons = append(reasons, m.GetLabel()[0].GetValue())
			if m.GetCounter().GetValue() != 0 {
				t.Fatalf("%v", m)
			}
		}
	}
	if strings.Join(reasons, ",") != "corrupt,manifest,missing,partial" {
		t.Fatalf("reasons %v", reasons)
	}
}

// 010-AC20: /metrics carries the device flags held now, the voice lines stored by source, those
// dropped as a repeat and those joined to a paragraph (018-AC9), one series per source.
func TestRegistryCountsVoiceSources(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := bus.New(st, web.NewToken())
	mfs, err := newRegistry(b, web.New(b, "", "http://x", web.Options{}, zap.NewNop())).Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			series := ""
			for _, l := range m.GetLabel() {
				series += l.GetName() + "=" + l.GetValue()
			}
			got[mf.GetName()] = append(got[mf.GetName()], series)
		}
	}
	for name, want := range map[string]string{
		"grill_room_device_stt_participants":   "",
		"grill_room_voice_lines_total":         "source=device,source=server",
		"grill_room_voice_lines_deduped_total": "source=device,source=server",
		// 018-AC9
		"grill_room_voice_lines_appended_total": "source=device,source=server",
	} {
		if s := strings.Join(got[name], ","); s != want || got[name] == nil {
			t.Errorf("%s: series %q, want %q", name, s, want)
		}
	}
}

// 012 Phase 2: /metrics carries one MCP tool-call series per tool and result.
func TestRegistryCountsMCPToolCalls(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := bus.New(st, web.NewToken())
	mfs, err := newRegistry(b, web.New(b, "", "http://x", web.Options{}, zap.NewNop())).Gather()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, mf := range mfs {
		if mf.GetName() == "grill_room_mcp_tool_calls_total" {
			n = len(mf.GetMetric())
		}
	}
	if n != len(web.MCPTools())*len(web.MCPResults) || n != 27*3 {
		t.Fatalf("%d series", n)
	}
}

// 017-AC20: the metrics tables of docs/architecture.md list exactly the room server's metrics: a
// registered one without a row, or a row naming none, fails.
func TestRegistryMetricsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	name := regexp.MustCompile("`(grill_room_[a-z_]+)")
	inTable := false
	for _, line := range strings.Split(string(doc), "\n") {
		switch {
		case strings.HasPrefix(line, "| Metric"):
			inTable = true
		case !strings.HasPrefix(line, "|"):
			inTable = false
		case inTable:
			cell, _, _ := strings.Cut(strings.TrimPrefix(line, "|"), "|")
			for _, m := range name.FindAllStringSubmatch(cell, -1) {
				documented[m[1]] = true
			}
		}
	}
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := bus.New(st, web.NewToken())
	opts := web.Options{TTSTokenHash: "x", Voice: &web.Voice{URL: "wss://x"}, OIDC: &web.OIDC{}}
	mfs, err := newRegistry(b, web.New(b, "", "http://x", opts, zap.NewNop())).Gather()
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, mf := range mfs {
		n := mf.GetName()
		if !strings.HasPrefix(n, "grill_room_") {
			continue
		}
		if !documented[n] {
			missing = append(missing, n)
		}
		delete(documented, n)
	}
	if len(missing) > 0 || len(documented) > 0 {
		t.Fatalf("metrics without a row in docs/architecture.md: %v; rows naming no registered metric: %v", missing, documented)
	}
}

// 021-AC1: the three provider settings go together, need an https public URL (localhost aside),
// and exclude anonymous rooms; unset, nothing else is asked for.
func TestValidateOIDC(t *testing.T) {
	full := web.OIDCConfig{Issuer: "https://idp.example", ClientID: "c", ClientSecret: "s", GroupsClaim: "groups"}
	for _, c := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"unset", Config{PublicURL: "http://rooms.example"}, true},
		{"all three, https", Config{PublicURL: "https://rooms.example", OIDC: full}, true},
		{"localhost over http", Config{PublicURL: "http://localhost:7680", OIDC: full}, true},
		{"127.0.0.1 over http", Config{PublicURL: "http://127.0.0.1:7680/", OIDC: full}, true},
		{"::1 over http", Config{PublicURL: "http://[::1]:7680", OIDC: full}, true},
		// The issuer serves the keys and receives the secret.
		{"issuer over http", Config{PublicURL: "https://rooms.example", OIDC: withIssuer(full, "http://idp.example")}, false},
		{"issuer over ftp", Config{PublicURL: "https://rooms.example", OIDC: withIssuer(full, "ftp://idp")}, false},
		{"issuer without scheme", Config{PublicURL: "https://rooms.example", OIDC: withIssuer(full, "idp.example")}, false},
		{"issuer on localhost over http", Config{PublicURL: "https://rooms.example", OIDC: withIssuer(full, "http://localhost:5556")}, true},
		{"plain http", Config{PublicURL: "http://rooms.example", OIDC: full}, false},
		{"localhost-like host", Config{PublicURL: "http://localhost.evil.example", OIDC: full}, false},
		{"not a URL", Config{PublicURL: "rooms.example", OIDC: full}, false},
		{"anonymous rooms", Config{PublicURL: "https://rooms.example", OIDC: full, AnonymousRooms: true}, false},
		{"issuer only", Config{PublicURL: "https://rooms.example", OIDC: web.OIDCConfig{Issuer: "https://idp.example"}}, false},
		{"no secret", Config{PublicURL: "https://rooms.example", OIDC: web.OIDCConfig{Issuer: "https://idp.example", ClientID: "c"}}, false},
		{"secret only", Config{PublicURL: "https://rooms.example", OIDC: web.OIDCConfig{ClientSecret: "s"}}, false},
		{"group without issuer", Config{PublicURL: "https://rooms.example", OIDC: web.OIDCConfig{MemberGroup: "staff"}}, false},
		{"scopes without issuer", Config{PublicURL: "https://rooms.example", OIDC: web.OIDCConfig{Scopes: []string{"x"}}}, false},
		// 021-AC8: the revalidation window, 1h to 720h, with an issuer only.
		{"revalidate without issuer", Config{PublicURL: "https://rooms.example", OIDC: web.OIDCConfig{Revalidate: time.Hour}, OIDCRevalidateSet: true}, false},
		{"revalidate 0 without issuer", Config{PublicURL: "https://rooms.example", OIDCRevalidateSet: true}, false},
		{"revalidate unset", Config{PublicURL: "https://rooms.example", OIDC: full}, true},
		{"revalidate 1h", revalidating(full, time.Hour), true},
		{"revalidate 720h", revalidating(full, 720*time.Hour), true},
		{"revalidate 0", revalidating(full, 0), false},
		{"revalidate under 1h", revalidating(full, time.Hour-time.Second), false},
		{"revalidate over 720h", revalidating(full, 720*time.Hour+time.Second), false},
		{"revalidate negative", revalidating(full, -time.Hour), false},
	} {
		if err := c.cfg.validateOIDC(); (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok %v", c.name, err, c.ok)
		}
	}
}

func withIssuer(c web.OIDCConfig, iss string) web.OIDCConfig { c.Issuer = iss; return c }

// revalidating is an https instance on c with GRILL_ROOM_OIDC_REVALIDATE set to d.
func revalidating(c web.OIDCConfig, d time.Duration) Config {
	c.Revalidate = d
	return Config{PublicURL: "https://rooms.example", OIDC: c, OIDCRevalidateSet: true}
}

// 021-AC1: the provider settings come from the environment, with their defaults.
func TestLoadConfigOIDC(t *testing.T) {
	t.Setenv("GRILL_ROOM_DATABASE_URL", "postgres://u@db/x")
	c, err := loadConfig()
	if want := (web.OIDCConfig{Scopes: []string{}, GroupsClaim: web.DefaultGroupsClaim}); err != nil || !reflect.DeepEqual(c.OIDC, want) {
		t.Fatalf("defaults: %+v %v", c.OIDC, err)
	}
	for k, v := range map[string]string{"GRILL_ROOM_OIDC_ISSUER": "https://idp.example", "GRILL_ROOM_OIDC_CLIENT_ID": "c", "GRILL_ROOM_OIDC_CLIENT_SECRET": "s",
		"GRILL_ROOM_OIDC_SCOPES": " groups  offline_access ", "GRILL_ROOM_OIDC_MEMBER_GROUP": "staff", "GRILL_ROOM_OIDC_GROUPS_CLAIM": "roles",
		"GRILL_ROOM_OIDC_REVALIDATE": "24h"} {
		t.Setenv(k, v)
	}
	c, err = loadConfig()
	want := web.OIDCConfig{Issuer: "https://idp.example", ClientID: "c", ClientSecret: "s", Scopes: []string{"groups", "offline_access"}, MemberGroup: "staff", GroupsClaim: "roles",
		Revalidate: 24 * time.Hour}
	if err != nil || !reflect.DeepEqual(c.OIDC, want) {
		t.Fatalf("set: %+v %v", c.OIDC, err)
	}
	if !c.OIDCRevalidateSet {
		t.Error("GRILL_ROOM_OIDC_REVALIDATE not marked set")
	}
	t.Setenv("GRILL_ROOM_OIDC_REVALIDATE", "0")
	if c, err := loadConfig(); err != nil || !c.OIDCRevalidateSet || c.validateOIDC() == nil {
		t.Errorf("GRILL_ROOM_OIDC_REVALIDATE=0 taken: %v", err)
	}
	t.Setenv("GRILL_ROOM_OIDC_REVALIDATE", "a week")
	if _, err := loadConfig(); err == nil {
		t.Error("an unparsable GRILL_ROOM_OIDC_REVALIDATE was taken")
	}
}

// 021-AC1: the start fetches the discovery document, retrying within the budget, and stops when
// it keeps failing or the start is cancelled; no issuer, no fetch. Without a member group it warns.
func TestConfigOIDCDiscovery(t *testing.T) {
	budget, backoff := discoveryBudget, discoveryBackoff
	t.Cleanup(func() { discoveryBudget, discoveryBackoff = budget, backoff })
	discoveryBudget, discoveryBackoff = 2*time.Second, 10*time.Millisecond
	core, logs := observer.New(zapcore.InfoLevel)
	log := zap.New(core)
	if o, err := (Config{}).oidc(t.Context(), log); o != nil || err != nil || logs.Len() != 0 {
		t.Fatalf("unset: %v %v %d logs", o, err, logs.Len())
	}
	var calls atomic.Int32
	failFirst := 0
	idp := &oidctest.Server{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(calls.Add(1)) <= failFirst {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		idp.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	idp.SetIssuer(ts.URL)
	c := Config{PublicURL: "https://rooms.example/", OIDC: web.OIDCConfig{Issuer: ts.URL, ClientID: "c", ClientSecret: "s"}}

	failFirst = 2
	if o, err := c.oidc(t.Context(), log); o == nil || err != nil || calls.Load() != 3 {
		t.Fatalf("two failures then up: %v %v after %d calls", o, err, calls.Load())
	}
	if logs.FilterMessage("oidc_discovery_failed").Len() != 2 || logs.FilterMessage("oidc_no_member_group").Len() != 1 {
		t.Errorf("logs %+v", logs.All())
	}

	calls.Store(0)
	failFirst = 1 << 30
	start := time.Now()
	if _, err := c.oidc(t.Context(), log); err == nil || calls.Load() < 3 || time.Since(start) > 4*time.Second {
		t.Fatalf("always down: %v after %d calls, %s", err, calls.Load(), time.Since(start))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start = time.Now()
	if _, err := c.oidc(ctx, log); err == nil || time.Since(start) > time.Second {
		t.Fatalf("cancelled start: %v, %s", err, time.Since(start))
	}

	logs.TakeAll()
	failFirst, c.OIDC.MemberGroup = 0, "staff"
	if _, err := c.oidc(t.Context(), log); err != nil || logs.FilterMessage("oidc_no_member_group").Len() != 0 {
		t.Fatalf("with a group: %v %+v", err, logs.All())
	}
}

// 021-AC13: restricted mode counts sign-ins by result; an open instance has no such series.
func TestRegistryCountsOIDCLogins(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := bus.New(st, web.NewToken())
	for _, c := range []struct {
		oidc *web.OIDC
		want int
	}{{nil, 0}, {&web.OIDC{}, len(web.OIDCResults)}} {
		mfs, err := newRegistry(b, web.New(b, "", "http://x", web.Options{OIDC: c.oidc}, zap.NewNop())).Gather()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, mf := range mfs {
			if mf.GetName() == "grill_room_oidc_logins_total" {
				n = len(mf.GetMetric())
			}
		}
		if n != c.want {
			t.Errorf("oidc %v: %d series, want %d", c.oidc != nil, n, c.want)
		}
	}
}
