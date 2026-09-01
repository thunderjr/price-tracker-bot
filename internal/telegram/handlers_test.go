package telegram

import (
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/thunderjr/price-tracker-bot/internal/store"
)

func TestParseTrackArgs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want store.WatchSpec
	}{
		{"ps5", store.WatchSpec{Query: "ps5"}},
		{"  LEGO   Millennium Falcon  ", store.WatchSpec{Query: "LEGO Millennium Falcon"}},

		// A price range is how the user says "the console, not its games".
		{"ps5 | min 3000", store.WatchSpec{Query: "ps5", MinCents: 300000}},
		{"ps5 | min 3000 | max 6000", store.WatchSpec{Query: "ps5", MinCents: 300000, MaxCents: 600000}},
		{"ps5 | minimo R$ 3.000,00", store.WatchSpec{Query: "ps5", MinCents: 300000}},

		{"ps5 | alvo 4200", store.WatchSpec{Query: "ps5", TargetCents: 420000}},
		{"ps5 | max 3.499,90", store.WatchSpec{Query: "ps5", MaxCents: 349990}},
		{"ps5 | ate 3500", store.WatchSpec{Query: "ps5", MaxCents: 350000}},

		{"ps5 | -portal", store.WatchSpec{Query: "ps5", Exclude: []string{"portal"}}},
		{"ps5 | -portal -ps4", store.WatchSpec{Query: "ps5", Exclude: []string{"portal", "ps4"}}},

		{
			"ps5 | min 3000 | alvo 4200 | -portal",
			store.WatchSpec{Query: "ps5", MinCents: 300000, TargetCents: 420000, Exclude: []string{"portal"}},
		},

		// An unrecognized clause must not swallow the query.
		{"ps5 | qualquer coisa", store.WatchSpec{Query: "ps5"}},
		{"", store.WatchSpec{}},
	} {
		got := ParseTrackArgs(tc.in)
		if got.Query != tc.want.Query {
			t.Errorf("ParseTrackArgs(%q).Query = %q, want %q", tc.in, got.Query, tc.want.Query)
		}
		if got.MinCents != tc.want.MinCents || got.MaxCents != tc.want.MaxCents {
			t.Errorf("ParseTrackArgs(%q) bounds = %d/%d, want %d/%d",
				tc.in, got.MinCents, got.MaxCents, tc.want.MinCents, tc.want.MaxCents)
		}
		if got.TargetCents != tc.want.TargetCents {
			t.Errorf("ParseTrackArgs(%q).TargetCents = %d, want %d", tc.in, got.TargetCents, tc.want.TargetCents)
		}
		if strings.Join(got.Exclude, ",") != strings.Join(tc.want.Exclude, ",") {
			t.Errorf("ParseTrackArgs(%q).Exclude = %v, want %v", tc.in, got.Exclude, tc.want.Exclude)
		}
	}
}

func TestListKeyboardPaginates(t *testing.T) {
	rows := make([]watchRow, 20)
	for i := range rows {
		rows[i] = watchRow{
			Watch: store.Watch{ID: int64(i + 1), Query: "q", Active: true},
			Stats: store.WatchStats{Products: 1, BestCents: 1000},
		}
	}

	kb := listKeyboard(rows, 0)
	// pageSize watch rows + nav row + actions row
	if got := len(kb.InlineKeyboard); got != pageSize+2 {
		t.Fatalf("page 0 has %d rows, want %d", got, pageSize+2)
	}

	nav := kb.InlineKeyboard[pageSize]
	if len(nav) != 2 {
		t.Fatalf("page 0 nav has %d buttons, want counter + next", len(nav))
	}
	if nav[0].Text != "1/3" {
		t.Errorf("page counter = %q, want 1/3", nav[0].Text)
	}

	// The last page is short and has no "next".
	last := listKeyboard(rows, 2)
	if got := len(last.InlineKeyboard); got != 4+2 {
		t.Fatalf("last page has %d rows, want %d", got, 4+2)
	}
	for _, btn := range last.InlineKeyboard[4] {
		cb, err := decode(btn.CallbackData)
		if err != nil {
			t.Fatal(err)
		}
		if cb.Action == actList && cb.Arg > 2 {
			t.Errorf("last page offers a next page: %+v", cb)
		}
	}
}

// An out-of-range page must fall back to the first one rather than panic.
func TestListKeyboardClampsPage(t *testing.T) {
	rows := []watchRow{{Watch: store.Watch{ID: 1, Query: "q", Active: true}}}
	for _, page := range []int64{-5, 99} {
		kb := listKeyboard(rows, page)
		if len(kb.InlineKeyboard) == 0 {
			t.Fatalf("page %d produced an empty keyboard", page)
		}
	}
}

func TestListKeyboardEmpty(t *testing.T) {
	kb := listKeyboard(nil, 0)
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatalf("empty list keyboard = %+v, want a single close button", kb.InlineKeyboard)
	}
	cb, err := decode(kb.InlineKeyboard[0][0].CallbackData)
	if err != nil || cb.Action != actClose {
		t.Errorf("empty list button = %+v (%v), want close", cb, err)
	}
}

// Every button must carry data this build can decode, or it hangs the client.
func TestAllKeyboardsDecodeCleanly(t *testing.T) {
	row := watchRow{
		Watch: store.Watch{ID: 7, Query: "ps5", Active: true, TargetCents: 350000},
		Stats: store.WatchStats{Products: 3, BestCents: 340000},
	}
	rows := []watchRow{row, {Watch: store.Watch{ID: 8, Query: "x"}}}

	for name, kb := range map[string]*models.InlineKeyboardMarkup{
		"list":    listKeyboard(rows, 0),
		"detail":  detailKeyboard(row, 0),
		"confirm": confirmDeleteKeyboard(7, 0),
		"back":    backKeyboard(7, 0),
	} {
		for _, r := range kb.InlineKeyboard {
			for _, btn := range r {
				if btn.Text == "" {
					t.Errorf("%s: button with empty label", name)
				}
				if len(btn.CallbackData) > 64 {
					t.Errorf("%s: callback data %q is %d bytes", name, btn.CallbackData, len(btn.CallbackData))
				}
				if _, err := decode(btn.CallbackData); err != nil {
					t.Errorf("%s: undecodable callback %q: %v", name, btn.CallbackData, err)
				}
			}
		}
	}
}

// The pause button must say what it will do, not what the state is.
func TestDetailKeyboardPauseLabelFollowsState(t *testing.T) {
	active := detailKeyboard(watchRow{Watch: store.Watch{ID: 1, Active: true}}, 0)
	if got := active.InlineKeyboard[1][0].Text; got != "⏸ Pausar" {
		t.Errorf("active watch pause button = %q", got)
	}
	paused := detailKeyboard(watchRow{Watch: store.Watch{ID: 1, Active: false}}, 0)
	if got := paused.InlineKeyboard[1][0].Text; got != "▶️ Retomar" {
		t.Errorf("paused watch pause button = %q", got)
	}
}

func TestWatchButtonLabel(t *testing.T) {
	got := watchButtonLabel(watchRow{
		Watch: store.Watch{Query: "ps5", Active: true, TargetCents: 350000},
		Stats: store.WatchStats{Products: 2, BestCents: 340000},
	})
	if want := "ps5 · R$ 3.400,00 🎯"; got != want {
		t.Errorf("label = %q, want %q", got, want)
	}

	paused := watchButtonLabel(watchRow{Watch: store.Watch{Query: "x"}})
	if paused != "x · — ⏸" {
		t.Errorf("paused empty label = %q", paused)
	}
}
