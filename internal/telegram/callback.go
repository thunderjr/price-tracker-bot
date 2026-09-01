package telegram

import (
	"fmt"
	"strconv"
	"strings"
)

// Telegram caps callback_data at 64 bytes, so the encoding is terse and never
// carries the query text -- only ids the handler resolves against the store.
//
// Every payload starts with a schema version. A redeploy that changes the
// scheme then makes old buttons fail cleanly instead of being misread.
const callbackVersion = "1"

// Actions carried in callback data.
const (
	actList        = "l"  // list view, arg = page
	actDetail      = "d"  // detail view for a watch
	actScan        = "s"  // scan one watch now
	actScanAll     = "sa" // scan every active watch
	actOffers      = "o"  // show current offers for a watch
	actTogglePause = "p"  // pause or resume
	actTarget      = "t"  // prompt for a target price
	actDeleteAsk   = "x"  // ask for confirmation
	actDeleteDo    = "xx" // confirmed delete
	actClose       = "c"  // dismiss the manager
	actSetFloor    = "f"  // apply a suggested price floor, arg = cents/100
	actNoop        = "-"  // inert button
)

// callback is a decoded button payload.
type callback struct {
	Action  string
	WatchID int64
	Arg     int64
}

// encode renders a payload. It panics on overflow rather than silently
// producing a button Telegram will reject at send time.
func encode(action string, watchID, arg int64) string {
	s := callbackVersion + ":" + action + ":" + strconv.FormatInt(watchID, 10) + ":" + strconv.FormatInt(arg, 10)
	if len(s) > 64 {
		panic("telegram: callback data over 64 bytes: " + s)
	}
	return s
}

// decode parses a payload. Button data is user-supplied: it can be replayed
// from an old message, or hand-crafted, so nothing here is trusted beyond its
// shape. The caller still has to check that the watch belongs to the chat.
func decode(data string) (callback, error) {
	parts := strings.Split(data, ":")
	if len(parts) != 4 {
		return callback{}, fmt.Errorf("telegram: malformed callback %q", data)
	}
	if parts[0] != callbackVersion {
		return callback{}, fmt.Errorf("telegram: callback schema %q is stale", parts[0])
	}

	watchID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return callback{}, fmt.Errorf("telegram: callback watch id %q: %w", parts[2], err)
	}
	arg, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return callback{}, fmt.Errorf("telegram: callback arg %q: %w", parts[3], err)
	}
	return callback{Action: parts[1], WatchID: watchID, Arg: arg}, nil
}
