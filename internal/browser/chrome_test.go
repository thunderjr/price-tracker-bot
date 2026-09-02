package browser

import (
	"context"
	"errors"
	"fmt"
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

// A Chromium that dies mid-run reports context.Canceled, because chromedp
// cancels the browser context when the websocket drops. Reading that as "the
// caller gave up" left the dead browser in place, and every search after it
// failed the same way.
func TestWorthRestartingAfterBrowserCrash(t *testing.T) {
	live := context.Background()

	if !worthRestarting(live, context.Canceled) {
		t.Error("a crashed browser was not restarted")
	}
	if !worthRestarting(live, errors.New("could not find node")) {
		t.Error("an ordinary failure was not restarted")
	}
	if worthRestarting(live, nil) {
		t.Error("a successful run restarted the browser")
	}

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if worthRestarting(gone, context.Canceled) {
		t.Error("relaunched Chromium after the caller gave up, which nobody is waiting for")
	}
}

// A failed run must be read correctly, because the two wrong answers are both
// costly: never restarting leaves every later scan talking to a dead browser,
// and restarting on a slow page throws away the warmed profile that gets us
// past Mercado Livre.
func TestWorthRestarting(t *testing.T) {
	live := context.Background()
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"nothing went wrong", live, nil, false},
		{"caller gave up", dead, context.Canceled, false},
		// chromedp cancels the browser context when the websocket drops, so a
		// crash arrives looking like an ordinary cancellation.
		{"chromium died mid-run", live, context.Canceled, true},
		{"page too slow", live, context.DeadlineExceeded, false},
		{"wrapped page too slow", live, fmt.Errorf("navigate: %w", context.DeadlineExceeded), false},
		{"chrome failed to start", live, errors.New("chrome failed to start"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := worthRestarting(tc.ctx, tc.err); got != tc.want {
				t.Errorf("worthRestarting(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
