// Command ptb tracks prices and promotions on Mercado Livre, Amazon Brazil and
// KaBuM!, and reports them through a Telegram bot.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/thunderjr/price-tracker-bot/internal/config"
)

const usage = `ptb - price tracker bot

Usage:
  ptb serve                      run the Telegram bot and the scan scheduler
  ptb probe <source> <query>     search one source now and print the results
                                 (source: amazon | meli | all | chrome)
  ptb watches [id]               list watches, or one watch's tracked listings
  ptb track [flags] <query>      add a watch
  ptb watch [flags] <id>         change a watch's query/min/max/target/exclude
  ptb scan [-notify] [id...]     scan for real; -notify posts to Telegram
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)

	switch args[0] {
	case "probe":
		return runProbe(cfg, args[1:])
	case "serve":
		return runServe(cfg)
	case "watches":
		return runWatches(cfg, args[1:])
	case "track":
		return runTrack(cfg, args[1:])
	case "watch":
		return runWatchSet(cfg, args[1:])
	case "scan":
		return runScan(cfg, args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}
