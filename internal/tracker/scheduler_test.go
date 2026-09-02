package tracker

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/store"
)

// hangingSource blocks until its context is cancelled, standing in for a
// source that has wedged.
type hangingSource struct{ name string }

func (h *hangingSource) Name() string { return h.name }

func (h *hangingSource) Search(ctx context.Context, _ string) ([]source.Offer, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// One wedged watch must not cost the rest of the sweep. Without a per-watch
// bound the sweep holds open on the first stuck source and every watch behind
// it goes unscanned until the next interval.
func TestSweepBoundsEachWatchSeparately(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	if _, err := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "wedged"}); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr := New(st, DefaultRules(0.10, 0.01), log, &hangingSource{name: "meli"})
	tr.pace = 0

	sched := NewScheduler(tr, st, nil, time.Hour, log)
	sched.watchTimeout = 100 * time.Millisecond

	done := make(chan struct{})
	go func() {
		sched.sweep(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not return: a wedged watch blocked it indefinitely")
	}
}
