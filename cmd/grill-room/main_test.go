package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

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
		AuditRetention: 365 * 24 * time.Hour, ClientRetention: 720 * time.Hour, NotesWatchDelay: 20 * time.Second}
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
		"sandbox digest too short": func(c *Config) {
			c.SandboxImage = "registry.example/grill-room/sandbox@sha256:" + strings.Repeat("a", 63)
		},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
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
	for key, val := range map[string]string{"GRILL_ROOM_MAX_UPLOAD_MB": "20MB", "GRILL_ROOM_MAX_ROOM_MB": "x", "GRILL_ROOM_PANDOC_TIMEOUT": "60", "GRILL_ROOM_MAX_ATTACHMENT_MB": "5MB", "GRILL_ROOM_TURN_TIMEOUT": "15", "GRILL_ROOM_MAGIC_LINK_TTL": "15", "GRILL_ROOM_AUDIT_RETENTION": "365", "GRILL_ROOM_CLIENT_RETENTION": "30", "GRILL_ROOM_TRUSTED_PROXIES": "10.0.0.0/33", "GRILL_ROOM_NOTES_WATCH_DELAY": "20"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, val)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("%s=%s accepted", key, val)
			}
		})
	}
	c, err := loadConfig()
	if err != nil || c.MaxUploadMB != 20 || c.MaxRoomMB != 200 || c.MaxAttachmentMB != 5 || c.TurnTimeout != 15*time.Minute || c.MagicLinkTTL != 15*time.Minute || c.AuditRetention != 8760*time.Hour || c.ClientRetention != 720*time.Hour || c.TrustedProxies != nil || c.NotesWatchDelay != 20*time.Second || c.Validate() != nil {
		t.Fatalf("defaults: %+v, %v", c, err)
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
	b := bus.New(st)
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

// 010-AC20: /metrics carries the device flags held now, the voice lines stored by source and those
// dropped as a repeat, one series per source.
func TestRegistryCountsVoiceSources(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := bus.New(st)
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
	b := bus.New(st)
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
