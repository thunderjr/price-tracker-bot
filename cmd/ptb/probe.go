package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/thunderjr/price-tracker-bot/internal/browser"
	"github.com/thunderjr/price-tracker-bot/internal/config"
	"github.com/thunderjr/price-tracker-bot/internal/relevance"
	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/source/amazon"
	"github.com/thunderjr/price-tracker-bot/internal/source/meli"
)

// runProbe hits one source live and prints what came back. This is the smoke
// test that tells us whether the container can still get past Mercado Livre's
// anti-bot protection -- run it after every base image or dependency bump.
func runProbe(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print every offer as JSON")
	limit := fs.Int("limit", 10, "how many offers to print (0 for all)")
	raw := fs.Bool("raw", false, "skip relevance filtering and show what the sites returned")
	exclude := fs.String("exclude", "", "comma-separated terms to drop")
	minPrice := fs.String("min", "", "drop offers below this price")
	maxPrice := fs.String("max", "", "drop offers above this price")
	intl := fs.Bool("international", false, "keep cross-border listings")
	require := fs.String("require", "", "comma-separated terms every title must contain")
	if err := fs.Parse(args); err != nil {
		return err
	}

	args = fs.Args()
	if len(args) < 2 {
		return errors.New("usage: ptb probe [-json] [-limit N] [-raw] <amazon|meli|all|chrome> <query>")
	}
	which, query := args[0], args[1]

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var sources []source.Source
	switch which {
	case "chrome":
		return probeChrome(ctx, cfg, query)
	case "amazon":
		sources = []source.Source{amazon.New(nil)}
	case "meli", "all":
		b := browser.New(browser.Options{
			ExecPath: cfg.ChromePath,
			// A separate profile: probing while the bot is running must not
			// steal its Chromium profile lock.
			ProfileDir: browser.ProbeProfileDir(cfg.ChromeProfile),
			Logger:     slog.Default(),
		})
		defer b.Close()

		sources = []source.Source{meli.New(b)}
		if which == "all" {
			sources = append(sources, amazon.New(nil))
		}
	default:
		return fmt.Errorf("unknown source %q (want amazon, meli, all or chrome)", which)
	}

	failed := false
	for _, s := range sources {
		start := time.Now()
		offers, err := s.Search(ctx, query)
		elapsed := time.Since(start).Round(time.Millisecond)

		if err != nil {
			failed = true
			if errors.Is(err, source.ErrBlocked) {
				fmt.Printf("\n%s: BLOCKED by anti-bot protection after %s\n", s.Name(), elapsed)
				continue
			}
			fmt.Printf("\n%s: FAILED after %s: %v\n", s.Name(), elapsed, err)
			continue
		}

		found := len(offers)
		var report *relevance.Report
		if !*raw {
			offers, report = relevance.Filter(query, offers, relevance.Options{
				Exclude:            splitTerms(*exclude),
				Require:            splitTerms(*require),
				MinCents:           source.ParseBRL(*minPrice),
				MaxCents:           source.ParseBRL(*maxPrice),
				AllowInternational: *intl,
			})
		}

		header := os.Stdout
		if *asJSON {
			header = os.Stderr
		}
		fmt.Fprintf(header, "\n%s: %d offers in %s", s.Name(), len(offers), elapsed)
		if report != nil && report.Dropped() > 0 {
			fmt.Fprintf(header, "  (%d of %d kept)", len(offers), found)
		}
		fmt.Fprintln(header)

		// With -json, stdout carries nothing but JSON so it can be piped;
		// the human-readable drop report goes to stderr.
		out := os.Stdout
		if *asJSON {
			out = os.Stderr

			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(offers); err != nil {
				return err
			}
		} else {
			printOffers(offers, *limit)
		}
		if report != nil {
			report.Print(out, *limit)
		}

		if found < 10 {
			fmt.Printf("%s: only %d offers before filtering -- selectors may have rotted\n", s.Name(), found)
			failed = true
		}
	}

	if failed {
		return errors.New("probe failed")
	}
	fmt.Fprintln(os.Stderr, "\nprobe ok")
	return nil
}

func splitTerms(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func printOffers(offers []source.Offer, limit int) {
	for i, o := range offers {
		if limit > 0 && i == limit {
			fmt.Printf("  ... and %d more\n", len(offers)-limit)
			break
		}
		line := fmt.Sprintf("  %-13s %11s", o.ExternalID, source.FormatBRL(o.PriceCents))
		if d := o.Discount(); d > 0 {
			line += fmt.Sprintf(" -%2d%%", d)
		} else {
			line += "     "
		}
		fmt.Printf("%s  %s\n", line, truncate(o.Title, 66))
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// probeChrome navigates to a raw URL and reports what the browser actually
// saw. It is the tool for telling "the selectors moved" apart from "we got an
// anti-bot page" apart from "Chromium never rendered anything".
func probeChrome(ctx context.Context, cfg *config.Config, url string) error {
	b := browser.New(browser.Options{
		ExecPath:   cfg.ChromePath,
		ProfileDir: browser.ProbeProfileDir(cfg.ChromeProfile),
		Logger:     slog.Default(),
	})
	defer b.Close()

	var title, ua, html string
	err := b.Run(ctx,
		chromedp.Navigate(url),
		chromedp.Sleep(6*time.Second),
		chromedp.Title(&title),
		chromedp.Evaluate(`navigator.userAgent + " | webdriver=" + navigator.webdriver`, &ua),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	)
	if err != nil {
		return fmt.Errorf("navigate %s: %w", url, err)
	}

	fmt.Printf("url:     %s\n", url)
	fmt.Printf("title:   %q\n", title)
	fmt.Printf("ua:      %s\n", ua)
	fmt.Printf("html:    %d bytes\n", len(html))
	fmt.Printf("blocked: %v\n", browser.IsBlockedPage(html))

	if out := os.Getenv("PTB_DUMP"); out != "" {
		if err := os.WriteFile(out, []byte(html), 0o644); err != nil {
			return err
		}
		fmt.Printf("dumped:  %s\n", out)
	}
	return nil
}
