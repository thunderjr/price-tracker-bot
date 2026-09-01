// Package config loads runtime settings from the environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds everything the bot needs to run.
type Config struct {
	TelegramToken  string
	AllowedChatIDs []int64

	DBPath        string
	ChromeProfile string
	ChromePath    string

	ScanInterval  time.Duration
	DropThreshold float64
	LogLevel      string
}

// Load reads the environment and applies defaults. It does not validate the
// Telegram token, so the probe subcommand can run without one.
func Load() (*Config, error) {
	dataDir := env("DATA_DIR", "/data")

	c := &Config{
		TelegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		DBPath:        env("DB_PATH", filepath.Join(dataDir, "ptb.db")),
		ChromeProfile: env("CHROME_PROFILE", filepath.Join(dataDir, "chrome-profile")),
		ChromePath:    os.Getenv("CHROME_PATH"),
		LogLevel:      env("LOG_LEVEL", "info"),
	}

	var err error
	if c.AllowedChatIDs, err = parseIDs(os.Getenv("ALLOWED_CHAT_IDS")); err != nil {
		return nil, fmt.Errorf("config: ALLOWED_CHAT_IDS: %w", err)
	}
	if c.ScanInterval, err = time.ParseDuration(env("SCAN_INTERVAL", "3h")); err != nil {
		return nil, fmt.Errorf("config: SCAN_INTERVAL: %w", err)
	}
	if c.ScanInterval < time.Minute {
		return nil, fmt.Errorf("config: SCAN_INTERVAL %s is too aggressive; use 1m or more", c.ScanInterval)
	}
	if c.DropThreshold, err = strconv.ParseFloat(env("DROP_THRESHOLD", "0.10"), 64); err != nil {
		return nil, fmt.Errorf("config: DROP_THRESHOLD: %w", err)
	}
	if c.DropThreshold <= 0 || c.DropThreshold >= 1 {
		return nil, fmt.Errorf("config: DROP_THRESHOLD must be between 0 and 1, got %v", c.DropThreshold)
	}
	return c, nil
}

// RequireBot returns an error when settings needed for the Telegram bot are
// missing. Without an allowlist anyone who finds the bot drives our scraper.
func (c *Config) RequireBot() error {
	if c.TelegramToken == "" {
		return fmt.Errorf("config: TELEGRAM_BOT_TOKEN is required")
	}
	if len(c.AllowedChatIDs) == 0 {
		return fmt.Errorf("config: ALLOWED_CHAT_IDS is required (comma-separated Telegram user IDs)")
	}
	return nil
}

// Allowed reports whether a chat may command the bot.
func (c *Config) Allowed(id int64) bool {
	for _, a := range c.AllowedChatIDs {
		if a == id {
			return true
		}
	}
	return false
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseIDs(raw string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a numeric chat id", part)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
