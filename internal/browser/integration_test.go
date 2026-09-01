//go:build integration

package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// tempProfile makes a profile directory whose cleanup tolerates Chromium still
// flushing on its way out. t.TempDir fails the test in that case.
func tempProfile(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ptb-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// Deriving a context from the allocator starts a whole new Chromium; only a
// context derived from the browser is a tab. Getting that wrong means every
// search launches its own browser against the shared profile, and the two
// fight over its lock -- which is exactly how this broke in production.
//
// Counting processes at the end would not catch it, because a per-run browser
// is also torn down per run. The PID has to be the same one throughout.
func TestReusesOneBrowserAcrossRuns(t *testing.T) {
	b := New(Options{
		ExecPath:   os.Getenv("CHROME_PATH"),
		ProfileDir: tempProfile(t),
		Timeout:    60 * time.Second,
	})
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var pids []string
	for i := range 3 {
		var title string
		if err := b.Run(ctx, chromedp.Navigate("https://example.com"), chromedp.Title(&title)); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if title != "Example Domain" {
			t.Fatalf("run %d: title = %q", i, title)
		}

		got := browserPIDs(t)
		if len(got) != 1 {
			t.Fatalf("run %d: %d browser processes alive, want 1: %v", i, len(got), got)
		}
		pids = append(pids, got[0])
	}

	for i, pid := range pids {
		if pid != pids[0] {
			t.Fatalf("run %d used chrome pid %s but run 0 used %s: the browser is being relaunched per call",
				i, pid, pids[0])
		}
	}
	t.Logf("three runs shared chrome pid %s", pids[0])
}

// browserPIDs lists live top-level Chromium processes. Renderers, zygotes, gpu
// and utility children all carry --type=; each browser also forks two crashpad
// handlers, which are helpers rather than browsers.
func browserPIDs(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("no /proc to inspect: %v", err)
	}

	var pids []string
	for _, e := range entries {
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if len(args) == 0 || !strings.Contains(args[0], "chrom") || strings.Contains(args[0], "crashpad") {
			continue
		}

		child := false
		for _, a := range args {
			if strings.HasPrefix(a, "--type=") {
				child = true
				break
			}
		}
		if !child {
			pids = append(pids, e.Name())
		}
	}
	return pids
}

// A container that dies without shutting Chromium down leaves a SingletonLock
// naming its hostname. The next container has a different hostname, so
// Chromium concludes another machine owns the profile and refuses to start --
// forever. This reproduces exactly that and proves we recover.
func TestRecoversFromForeignProfileLock(t *testing.T) {
	profile := tempProfile(t)
	// Verbatim shape of the lock a dead container left in the real volume.
	if err := os.Symlink("0cd9aae89cd6-47", filepath.Join(profile, "SingletonLock")); err != nil {
		t.Fatal(err)
	}

	b := New(Options{
		ExecPath:   os.Getenv("CHROME_PATH"),
		ProfileDir: profile,
		Timeout:    60 * time.Second,
	})
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var title string
	if err := b.Run(ctx, chromedp.Navigate("https://example.com"), chromedp.Title(&title)); err != nil {
		t.Fatalf("browser did not recover from a foreign profile lock: %v", err)
	}
	if title != "Example Domain" {
		t.Errorf("title = %q, want %q", title, "Example Domain")
	}
}

// The profile has to survive a restart, or Mercado Livre sees a brand new
// visitor on every deploy.
func TestProfilePersistsAcrossRestarts(t *testing.T) {
	profile := tempProfile(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	run := func() {
		b := New(Options{
			ExecPath:   os.Getenv("CHROME_PATH"),
			ProfileDir: profile,
			Timeout:    60 * time.Second,
		})
		defer b.Close()

		if err := b.Run(ctx, chromedp.Navigate("https://example.com")); err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	run()
	run() // second launch must not trip over the first launch's lock

	// Chromium flushes the profile as it exits, and Close only cancels the
	// allocator, so give the write a moment to land.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(profile, "Default")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("profile data was never written to %s", profile)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
