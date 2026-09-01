package telegram

import "testing"

func TestCallbackRoundTrip(t *testing.T) {
	for _, want := range []callback{
		{Action: actList, Arg: 3},
		{Action: actDetail, WatchID: 42},
		{Action: actDeleteDo, WatchID: 999999999},
		{Action: actScanAll},
	} {
		got, err := decode(encode(want.Action, want.WatchID, want.Arg))
		if err != nil {
			t.Fatalf("decode(encode(%+v)): %v", want, err)
		}
		if got != want {
			t.Errorf("round trip = %+v, want %+v", got, want)
		}
	}
}

// Telegram silently rejects a keyboard whose callback_data is too long, so the
// budget has to hold for realistic ids.
func TestEncodeFitsTelegramLimit(t *testing.T) {
	const maxInt64 = 9223372036854775807
	if got := len(encode(actDeleteDo, maxInt64, maxInt64)); got > 64 {
		t.Errorf("encoded length %d exceeds Telegram's 64 byte limit", got)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for _, data := range []string{
		"",
		"1:d:42",           // too few fields
		"1:d:42:0:extra",   // too many
		"9:d:42:0",         // stale schema version
		"1:d:abc:0",        // non-numeric watch id
		"1:d:42:xyz",       // non-numeric arg
		"'; DROP TABLE --", // not a payload at all
	} {
		if _, err := decode(data); err == nil {
			t.Errorf("decode(%q) succeeded, want an error", data)
		}
	}
}
