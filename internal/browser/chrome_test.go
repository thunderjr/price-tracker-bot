package browser

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestIsBlockedPage(t *testing.T) {
	for name, page := range map[string]string{
		"error page":      `<html><body>Hubo un error accediendo a esta pagina...</body></html>`,
		"traffic wall":    `<html data-assets-prefix="https://x/suspicious-traffic-frontend/"></html>`,
		"verify redirect": `<html><a href="/gz/account-verification?go=x">verify</a></html>`,
	} {
		if !IsBlockedPage(page) {
			t.Errorf("%s: not detected as blocked", name)
		}
	}

	if IsBlockedPage(`<html><ol class="ui-search-layout"><li>real result</li></ol></html>`) {
		t.Error("a real results page was reported as blocked")
	}
}

// Chromium's profile lock names the host that took it. In a container that
// host is gone on the next start, and Chromium then refuses to run forever, so
// the lock has to be cleared before every launch.
func TestClearStaleLocks(t *testing.T) {
	dir := t.TempDir()

	// Reproduce what a killed container leaves behind: two dangling symlinks
	// and a leftover socket path.
	for name, target := range map[string]string{
		"SingletonLock":   "0cd9aae89cd6-47",
		"SingletonCookie": "146487179482269197",
		"SingletonSocket": "/tmp/org.chromium.Chromium.7mXMTh/SingletonSocket",
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatalf("setup %s: %v", name, err)
		}
	}
	keep := filepath.Join(dir, "Cookies")
	if err := os.WriteFile(keep, []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := New(Options{
		ProfileDir: dir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	b.clearStaleLocks()

	for _, name := range singletonFiles {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", name, err)
		}
	}
	// Everything else in the profile is the reason we keep it across restarts.
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("clearStaleLocks deleted real profile data: %v", err)
	}
}

func TestClearStaleLocksOnCleanProfile(t *testing.T) {
	b := New(Options{
		ProfileDir: t.TempDir(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	b.clearStaleLocks() // must not panic or error on a profile with no locks
}

func TestProbeProfileDir(t *testing.T) {
	if got := ProbeProfileDir("/data/chrome-profile"); got != "/data/chrome-profile-probe" {
		t.Errorf("ProbeProfileDir = %q", got)
	}
	if got := ProbeProfileDir(""); got != "" {
		t.Errorf("ProbeProfileDir(\"\") = %q, want empty so chromedp picks a temp dir", got)
	}
}
