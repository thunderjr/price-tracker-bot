// Package browser owns the single long-lived Chrome instance the scrapers share.
//
// Mercado Livre serves an anti-bot page ("Hubo un error accediendo a esta
// pagina") to every headless Chrome, including --headless=new, regardless of
// stealth flags. It serves real results to a headful browser over the exact
// same CDP connection. So this package always runs headful, and in Docker that
// means an Xvfb display -- see docker-entrypoint.sh.
package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// blockedMarkers are the fingerprints of Mercado Livre's two interstitials.
var blockedMarkers = []string{
	"Hubo un error accediendo a esta pagina",
	"suspicious-traffic",
	"account-verification",
}

// BlockedMarkers returns the interstitial fingerprints, so a scraper checking
// the same thing in-page cannot drift from this list.
func BlockedMarkers() []string {
	return slices.Clone(blockedMarkers)
}

// IsBlockedPage reports whether page content is an anti-bot interstitial
// rather than real results.
func IsBlockedPage(html string) bool {
	for _, m := range blockedMarkers {
		if strings.Contains(html, m) {
			return true
		}
	}
	return false
}

// Options configures the managed Chrome.
type Options struct {
	// ExecPath is the Chrome/Chromium binary. Empty lets chromedp look it up.
	ExecPath string
	// ProfileDir persists cookies and site reputation across restarts. Losing
	// it means starting over as an unknown visitor on every deploy.
	ProfileDir string
	// Lang is the Accept-Language / UI locale, e.g. "pt-BR".
	Lang string
	// Timeout bounds a single Run call.
	Timeout time.Duration

	Logger *slog.Logger
}

func (o *Options) setDefaults() {
	if o.Lang == "" {
		o.Lang = "pt-BR"
	}
	if o.Timeout == 0 {
		o.Timeout = 90 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Browser is a lazily started, self-healing Chrome. It is safe for concurrent
// use, but serializes work: one page navigation at a time keeps our traffic
// shaped like a person's.
type Browser struct {
	opts Options

	mu         sync.Mutex
	allocStop  context.CancelFunc
	browserCtx context.Context
	browserOff context.CancelFunc
}

// New returns a Browser. Chrome is not launched until the first Run.
func New(opts Options) *Browser {
	opts.setDefaults()
	return &Browser{opts: opts}
}

// Close shuts Chrome down.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopLocked()
}

func (b *Browser) stopLocked() {
	// Cancel the browser context first: chromedp.Cancel asks Chromium to close
	// and waits for it. Killing the allocator instead leaves the profile lock
	// and a half-written profile behind, which is what makes the next start
	// fail.
	if b.browserCtx != nil {
		if err := chromedp.Cancel(b.browserCtx); err != nil {
			b.opts.Logger.Debug("chrome did not exit cleanly", "err", err)
		}
		b.browserOff()
		b.browserCtx, b.browserOff = nil, nil
	}
	if b.allocStop != nil {
		b.allocStop()
		b.allocStop = nil
	}
}

func (b *Browser) startLocked() error {
	if b.browserCtx != nil && b.browserCtx.Err() == nil {
		return nil
	}
	// A browser that died on its own leaves a cancelled context behind.
	b.stopLocked()

	if b.opts.ProfileDir != "" {
		if err := os.MkdirAll(b.opts.ProfileDir, 0o755); err != nil {
			return fmt.Errorf("browser: profile dir: %w", err)
		}
		b.clearStaleLocks()
	}

	// Start from an empty flag set rather than chromedp's defaults, which
	// include --headless and --enable-automation.
	flags := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("headless", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("lang", b.opts.Lang),
		chromedp.Flag("accept-lang", b.opts.Lang),
		chromedp.Flag("window-size", "1440,900"),
		chromedp.Flag("window-position", "0,0"),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("no-service-autorun", true),
		chromedp.Flag("password-store", "basic"),
	}
	if b.opts.ExecPath != "" {
		flags = append(flags, chromedp.ExecPath(b.opts.ExecPath))
	}
	if b.opts.ProfileDir != "" {
		flags = append(flags, chromedp.UserDataDir(b.opts.ProfileDir))
	}

	allocCtx, allocStop := chromedp.NewExecAllocator(context.Background(), flags...)

	// One browser context for the whole process. Tabs are derived from this;
	// deriving them from the allocator instead would start a separate Chromium
	// per call, and two Chromiums on one profile fight over its lock.
	browserCtx, browserOff := chromedp.NewContext(allocCtx)

	// NewContext is lazy -- an empty Run is what actually launches Chromium,
	// so a failure to start surfaces here rather than inside the first scan.
	if err := chromedp.Run(browserCtx); err != nil {
		browserOff()
		allocStop()
		return fmt.Errorf("browser: start chrome: %w", err)
	}

	b.allocStop, b.browserCtx, b.browserOff = allocStop, browserCtx, browserOff
	b.opts.Logger.Info("chrome started",
		"exec", b.opts.ExecPath, "profile", b.opts.ProfileDir, "display", os.Getenv("DISPLAY"))
	return nil
}

// singletonFiles are the profile locks Chromium leaves behind when it does not
// exit cleanly.
var singletonFiles = []string{"SingletonLock", "SingletonCookie", "SingletonSocket"}

// clearStaleLocks removes Chromium's profile lock.
//
// The lock records the hostname that took it ("SingletonLock -> 4f2a1c-47").
// In a container every restart brings a new hostname, so Chromium decides
// another machine owns the profile and refuses to start -- permanently, and a
// relaunch cannot break the tie. Any unclean shutdown would otherwise take
// Mercado Livre out for good.
//
// Removing these is safe because this process owns the profile directory and
// never runs two browsers against it at once. One-off commands are given their
// own profile (see ProbeProfileDir) so they cannot collide with a running bot.
func (b *Browser) clearStaleLocks() {
	for _, name := range singletonFiles {
		path := filepath.Join(b.opts.ProfileDir, name)
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			b.opts.Logger.Warn("could not clear stale chrome lock", "path", path, "err", err)
			continue
		}
		b.opts.Logger.Info("cleared stale chrome profile lock", "path", path)
	}
}

// ProbeProfileDir derives a sibling profile for one-off commands, so a manual
// probe never fights the long-running bot over the same lock.
func ProbeProfileDir(profileDir string) string {
	if profileDir == "" {
		return ""
	}
	return profileDir + "-probe"
}

// Run executes actions in a fresh tab. If the first attempt fails, Chrome is
// torn down and relaunched once -- a crashed browser is the single most common
// long-running failure, and it is always fixed by a restart.
func (b *Browser) Run(ctx context.Context, actions ...chromedp.Action) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	err := b.runOnceLocked(ctx, actions...)
	if err == nil || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return err
	}

	b.opts.Logger.Warn("chrome run failed, restarting browser", "err", err)
	b.stopLocked()
	if err := b.runOnceLocked(ctx, actions...); err != nil {
		return fmt.Errorf("browser: after restart: %w", err)
	}
	return nil
}

func (b *Browser) runOnceLocked(ctx context.Context, actions ...chromedp.Action) error {
	if err := b.startLocked(); err != nil {
		return err
	}

	// A fresh tab on the existing browser, so each search starts from a clean
	// page without paying to launch Chromium again.
	tabCtx, cancelTab := chromedp.NewContext(b.browserCtx)
	defer cancelTab()

	runCtx, cancelTimeout := context.WithTimeout(tabCtx, b.opts.Timeout)
	defer cancelTimeout()

	return chromedp.Run(runCtx, actions...)
}
