package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATA_DIR", "/tmp/ptb")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != "/tmp/ptb/ptb.db" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.ChromeProfile != "/tmp/ptb/chrome-profile" {
		t.Errorf("ChromeProfile = %q", cfg.ChromeProfile)
	}
	if cfg.ScanInterval != 3*time.Hour {
		t.Errorf("ScanInterval = %v", cfg.ScanInterval)
	}
	if cfg.DropThreshold != 0.10 {
		t.Errorf("DropThreshold = %v", cfg.DropThreshold)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"non-numeric chat id": {"ALLOWED_CHAT_IDS": "123,abc"},
		"unparsable interval": {"SCAN_INTERVAL": "soon"},
		"hammering interval":  {"SCAN_INTERVAL": "5s"},
		"threshold over one":  {"DROP_THRESHOLD": "1.5"},
		"threshold at zero":   {"DROP_THRESHOLD": "0"},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Errorf("Load accepted %v", env)
			}
		})
	}
}

func TestRequireBot(t *testing.T) {
	cfg := &Config{}
	if err := cfg.RequireBot(); err == nil {
		t.Error("RequireBot passed with no token")
	}

	cfg.TelegramToken = "x"
	if err := cfg.RequireBot(); err == nil {
		t.Error("RequireBot passed with an empty allowlist; the bot would answer anyone")
	}

	cfg.AllowedChatIDs = []int64{1}
	if err := cfg.RequireBot(); err != nil {
		t.Errorf("RequireBot: %v", err)
	}
}

func TestAllowed(t *testing.T) {
	cfg := &Config{AllowedChatIDs: []int64{10, 20}}
	if !cfg.Allowed(20) {
		t.Error("allowlisted chat rejected")
	}
	if cfg.Allowed(30) {
		t.Error("unknown chat allowed")
	}
	if (&Config{}).Allowed(10) {
		t.Error("empty allowlist allowed a chat")
	}
}

func TestParseIDs(t *testing.T) {
	got, err := parseIDs(" 123 , 456,, -100200 ")
	if err != nil {
		t.Fatalf("parseIDs: %v", err)
	}
	want := []int64{123, 456, -100200} // group chat ids are negative
	if len(got) != len(want) {
		t.Fatalf("parseIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseIDs[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}
