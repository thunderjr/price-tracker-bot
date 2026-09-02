package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/browser"
	"github.com/thunderjr/price-tracker-bot/internal/config"
	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/source/amazon"
	"github.com/thunderjr/price-tracker-bot/internal/source/meli"
	"github.com/thunderjr/price-tracker-bot/internal/store"
	"github.com/thunderjr/price-tracker-bot/internal/telegram"
	"github.com/thunderjr/price-tracker-bot/internal/tracker"
)

// runWatches lists every watch with the filters it applies, or the tracked
// listings of one watch when given an id.
func runWatches(cfg *config.Config, args []string) error {
	if len(args) == 1 {
		return runWatchOffers(cfg, args[0])
	}
	return listWatches(cfg)
}

// runWatchOffers prints everything one watch is currently tracking, cheapest
// first, which is how you spot junk that a filter should be removing.
func runWatchOffers(cfg *config.Config, arg string) error {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return fmt.Errorf("watch id %q: %w", arg, err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	w, err := st.Watch(ctx, id)
	if err != nil {
		return err
	}
	offers, err := st.WatchOffers(ctx, id, w.PriceMode, 30*24*time.Hour, 1000)
	if err != nil {
		return err
	}

	fmt.Printf("watch %d %q: %d tracked (min=%s max=%s exclude=%s)\n\n",
		w.ID, w.Query, len(offers), cents(w.MinCents), cents(w.MaxCents),
		dash(strings.Join(w.Exclude, ",")))
	for _, o := range offers {
		fmt.Printf("  %-6s %-13s %11s  %s\n",
			o.Source, o.ExternalID, source.FormatBRL(o.Effective()), truncate(o.Title, 60))
	}
	return nil
}

func listWatches(cfg *config.Config) error {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	watches, err := allWatches(ctx, st, cfg)
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		fmt.Println("no watches")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tQUERY\tMIN\tMAX\tTARGET\tREQUIRE\tEXCLUDE\tINTL\tMODE\tSTATE\tOFFERS\tBEST\tLAST SCAN")
	for _, watch := range watches {
		stats, err := st.Stats(ctx, watch.ID, watch.PriceMode, 30*24*time.Hour)
		if err != nil {
			return err
		}

		state := "active"
		if !watch.Active {
			state = "paused"
		}
		best := "-"
		if stats.Products > 0 {
			best = source.FormatBRL(stats.BestCents)
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			watch.ID, watch.Query,
			cents(watch.MinCents), cents(watch.MaxCents), cents(watch.TargetCents),
			dash(strings.Join(watch.Require, ",")), dash(strings.Join(watch.Exclude, ",")),
			yesNo(watch.AllowInternational), modeLabel(watch.PriceMode),
			state, stats.Products, best, lastScan(watch.LastScanAt))
	}
	return w.Flush()
}

// runWatchSet edits one watch's filters.
func runWatchSet(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	minPrice := fs.String("min", "", "only track offers at or above this price (0 clears)")
	maxPrice := fs.String("max", "", "only track offers at or below this price (0 clears)")
	target := fs.String("target", "", "alert when the price reaches this (0 clears)")
	exclude := fs.String("exclude", "", "comma-separated title terms to drop (empty string clears)")
	intl := fs.Bool("international", false, "keep cross-border listings (their price excludes import tax)")
	query := fs.String("query", "", "change the search query, keeping the watch and its history")
	require := fs.String("require", "", "comma-separated terms every title must contain (empty clears)")
	baseline := fs.String("baseline", "", "best price to measure the next move against (0 re-baselines on the next scan)")
	mode := fs.String("mode", "", `which price to rank and alert on: "avista" or "parcelado"`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ptb watch [-min X] [-max Y] [-target Z] [-exclude a,b] [-mode avista|parcelado] <watch id>")
	}

	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("watch id %q: %w", fs.Arg(0), err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	w, err := st.Watch(ctx, id)
	if err != nil {
		return err
	}

	// Only touch what was actually passed, so setting a min does not silently
	// clear an existing max.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if set["min"] || set["max"] {
		minCents, maxCents := w.MinCents, w.MaxCents
		if set["min"] {
			minCents = source.ParseBRL(*minPrice)
		}
		if set["max"] {
			maxCents = source.ParseBRL(*maxPrice)
		}
		if err := st.SetWatchBounds(ctx, id, minCents, maxCents); err != nil {
			return err
		}
	}
	if set["target"] {
		if err := st.SetWatchTarget(ctx, id, source.ParseBRL(*target)); err != nil {
			return err
		}
	}
	if set["exclude"] {
		if err := st.SetWatchExclude(ctx, id, splitTerms(*exclude)); err != nil {
			return err
		}
	}
	if set["baseline"] {
		if err := st.SetNotifiedBest(ctx, id, source.ParseBRL(*baseline)); err != nil {
			return err
		}
	}
	if set["require"] {
		if err := st.SetWatchRequire(ctx, id, splitTerms(*require)); err != nil {
			return err
		}
	}
	if set["query"] {
		if err := st.RenameWatch(ctx, id, *query); err != nil {
			return err
		}
	}
	if set["international"] {
		if err := st.SetWatchInternational(ctx, id, *intl); err != nil {
			return err
		}
	}
	if set["mode"] {
		m, err := store.ParsePriceMode(*mode)
		if err != nil {
			return err
		}
		if err := st.SetWatchPriceMode(ctx, id, m); err != nil {
			return err
		}
	}

	updated, err := st.Watch(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("watch %d %q: min=%s max=%s target=%s require=%s exclude=%s international=%v baseline=%s mode=%s\n",
		updated.ID, updated.Query,
		cents(updated.MinCents), cents(updated.MaxCents), cents(updated.TargetCents),
		dash(strings.Join(updated.Require, ",")), dash(strings.Join(updated.Exclude, ",")),
		updated.AllowInternational, cents(updated.NotifiedBestCents), modeLabel(updated.PriceMode))
	return nil
}

// modeLabel names a price mode for the CLI's own output.
func modeLabel(m store.PriceMode) string {
	if m == store.ModeInstallment {
		return "parcelado"
	}
	return "avista"
}

// runTrack adds a watch from the command line, using the same argument
// grammar as the Telegram /track command.
func runTrack(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("track", flag.ContinueOnError)
	chat := fs.Int64("chat", 0, "chat id to own the watch (defaults to the first allowed chat)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New(`usage: ptb track [-chat ID] "<query> | min 3000 | -termo"`)
	}

	chatID := *chat
	if chatID == 0 {
		if len(cfg.AllowedChatIDs) == 0 {
			return errors.New("no ALLOWED_CHAT_IDS configured; pass -chat")
		}
		chatID = cfg.AllowedChatIDs[0]
	}

	spec := telegram.ParseTrackArgs(strings.Join(fs.Args(), " "))
	if spec.Query == "" {
		return errors.New("empty query")
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	w, err := st.CreateWatch(context.Background(), chatID, spec)
	if err != nil {
		return err
	}
	fmt.Printf("watch %d %q for chat %d: min=%s max=%s target=%s exclude=%s international=%v\n",
		w.ID, w.Query, w.ChatID, cents(w.MinCents), cents(w.MaxCents), cents(w.TargetCents),
		dash(strings.Join(w.Exclude, ",")), w.AllowInternational)
	return nil
}

// runScan scans watches for real and optionally posts the result to Telegram.
func runScan(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	notify := fs.Bool("notify", false, "send the results and any alerts to the watch's Telegram chat")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	watches, err := selectWatches(ctx, st, fs.Args())
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		return errors.New("no matching watches")
	}

	chrome := browser.New(browser.Options{
		ExecPath:   cfg.ChromePath,
		ProfileDir: browser.ProbeProfileDir(cfg.ChromeProfile),
		Logger:     slog.Default(),
	})
	defer chrome.Close()

	trk := tracker.New(st, tracker.DefaultRules(cfg.DropThreshold, cfg.BestMoveThreshold), slog.Default(),
		meli.New(chrome), amazon.New(nil))

	var bot *telegram.Bot
	if *notify {
		if err := cfg.RequireBot(); err != nil {
			return err
		}
		if bot, err = telegram.New(cfg, st, trk, slog.Default()); err != nil {
			return err
		}
	}

	for i, w := range watches {
		// Amazon answers roughly four searches a minute with a captcha, so
		// several watches back to back is exactly the burst that trips it.
		if i > 0 {
			if err := sleepCtx(ctx, betweenWatches); err != nil {
				return err
			}
		}

		res, err := trk.Scan(ctx, w)
		if err != nil {
			return fmt.Errorf("scan %q: %w", w.Query, err)
		}

		// "found" is what this scan matched; "tracking" is the size of the
		// watch. They differ whenever a source fails -- its listings stay
		// tracked from before -- and conflating them reads as data loss.
		fmt.Printf("watch %d %q\n", w.ID, w.Query)
		fmt.Printf("  found %d this scan · filtered %d · pruned %d · now tracking %d · %d alerts\n",
			res.Found, res.Filtered, res.Pruned, res.Tracked, len(res.Alerts))
		if res.Suggestion > 0 {
			fmt.Printf("  suggested floor: %s\n", source.FormatBRL(res.Suggestion))
		}
		for name, err := range res.Skipped {
			fmt.Printf("  ⚠ %s did not answer: %v\n", name, err)
		}

		offers, err := st.WatchOffers(ctx, w.ID, w.PriceMode, 30*24*time.Hour, 10)
		if err != nil {
			return err
		}
		// The figure the watch ranked on, so this list reads in the order it
		// is printed in. In "parcelado" mode the cash price follows it.
		for _, o := range offers {
			line := fmt.Sprintf("  %-6s %11s", o.Source, source.FormatBRL(o.Effective()))
			if o.Effective() != o.PriceCents {
				line += fmt.Sprintf("  (%s à vista)", source.FormatBRL(o.PriceCents))
			}
			fmt.Printf("%s  %s\n", line, truncate(o.Title, 52))
		}

		if bot == nil {
			continue
		}
		// ReportScan already carries this scan's alerts, so delivering them
		// again would put two near-identical messages in the chat.
		if err := bot.ReportScan(ctx, w, res); err != nil {
			return fmt.Errorf("report %q: %w", w.Query, err)
		}
		fmt.Printf("  sent to chat %d\n", w.ChatID)
	}
	return nil
}

// betweenWatches paces a multi-watch run.
const betweenWatches = 15 * time.Second

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// selectWatches resolves command arguments to watches: ids, "all", or nothing
// for every active watch.
func selectWatches(ctx context.Context, st *store.Store, args []string) ([]store.Watch, error) {
	if len(args) == 0 || (len(args) == 1 && args[0] == "all") {
		return st.ActiveWatches(ctx)
	}

	var out []store.Watch
	for _, arg := range args {
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("watch id %q: %w", arg, err)
		}
		w, err := st.Watch(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, nil
}

// allWatches gathers every watch across the allowed chats, since the store
// scopes listing by chat.
func allWatches(ctx context.Context, st *store.Store, cfg *config.Config) ([]store.Watch, error) {
	seen := map[int64]bool{}
	var out []store.Watch

	for _, chatID := range cfg.AllowedChatIDs {
		watches, err := st.Watches(ctx, chatID)
		if err != nil {
			return nil, err
		}
		for _, w := range watches {
			if !seen[w.ID] {
				seen[w.ID] = true
				out = append(out, w)
			}
		}
	}

	// Anything belonging to a chat no longer on the allowlist still shows up,
	// so a stale watch cannot hide from this listing.
	active, err := st.ActiveWatches(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range active {
		if !seen[w.ID] {
			seen[w.ID] = true
			out = append(out, w)
		}
	}
	return out, nil
}

func cents(v int64) string {
	if v == 0 {
		return "-"
	}
	return source.FormatBRL(v)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func lastScan(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("15:04 02/01")
}
