// Package store persists watches, products and their price history in SQLite.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// migrations bring an existing database up to the current schema. SQLite has
// no "ADD COLUMN IF NOT EXISTS", and the schema above only runs for a fresh
// file, so each of these is applied and its "duplicate column" error ignored.
var migrations = []string{
	`ALTER TABLE watches ADD COLUMN min_cents INTEGER`,
	`ALTER TABLE watches ADD COLUMN max_cents INTEGER`,
	`ALTER TABLE watches ADD COLUMN exclude_terms TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE products ADD COLUMN international INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE watches ADD COLUMN allow_international INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE watches ADD COLUMN require_terms TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE watches ADD COLUMN notified_best_cents INTEGER NOT NULL DEFAULT 0`,
}

// ErrNotFound is returned when a lookup by id matches nothing.
var ErrNotFound = errors.New("store: not found")

// Watch is one tracked query belonging to one chat.
type Watch struct {
	ID          int64
	ChatID      int64
	Query       string
	TargetCents int64 // 0 means no target
	// MinCents and MaxCents bound which listings this watch tracks at all.
	// They are the user's way of saying "a PS5 console, not a PS5 game" --
	// something no title heuristic can decide safely.
	MinCents int64
	MaxCents int64
	// Exclude drops listings whose title contains any of these terms.
	Exclude []string
	// Require drops listings whose title lacks any of these terms.
	Require []string
	// NotifiedBestCents is the best price the user was last told about. It is
	// what makes "the best price moved" notifiable exactly once per move,
	// instead of on every scan that still sees the new price.
	NotifiedBestCents int64
	// AllowInternational keeps cross-border listings, which are dropped by
	// default because their price excludes Brazilian import tax.
	AllowInternational bool
	Active             bool
	CreatedAt          time.Time
	LastScanAt         time.Time // zero when never scanned
}

// Product is a listing identified stably across scans.
type Product struct {
	ID         int64
	Source     string
	ExternalID string
	Title      string
	URL        string
	ImageURL   string
	Seller     string
	// International marks a cross-border listing, whose price excludes
	// Brazilian import tax. Stored so a watch can drop one it picked up
	// before that filter existed.
	International bool
}

// PricePoint is one observation of a product's price.
type PricePoint struct {
	ProductID      int64
	PriceCents     int64
	ListPriceCents int64
	SiteFlags      []string
	SeenAt         time.Time
}

// Store is the database handle.
type Store struct {
	db *sql.DB
}

// Open connects to the SQLite database at path, creating it and applying the
// schema if needed.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// One writer: SQLite serializes writes anyway, and this keeps "database is
	// locked" out of the picture entirely.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema to %s: %w", filepath.Base(path), err)
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("store: migrate %s: %w", filepath.Base(path), err)
		}
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// --- watches ---

// WatchSpec is what the user asked to track.
type WatchSpec struct {
	Query              string
	Require            []string
	TargetCents        int64
	MinCents           int64
	MaxCents           int64
	Exclude            []string
	AllowInternational bool
}

// CreateWatch adds a watch, or reactivates and updates one the chat had before.
func (s *Store) CreateWatch(ctx context.Context, chatID int64, spec WatchSpec) (*Watch, error) {
	query := strings.Join(strings.Fields(spec.Query), " ")
	if query == "" {
		return nil, errors.New("store: empty query")
	}

	now := formatTime(time.Now())
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO watches (chat_id, query, target_cents, min_cents, max_cents, exclude_terms,
		                     require_terms, allow_international, active, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT (chat_id, query) DO UPDATE SET
		    active              = 1,
		    target_cents        = excluded.target_cents,
		    min_cents           = excluded.min_cents,
		    max_cents           = excluded.max_cents,
		    exclude_terms       = excluded.exclude_terms,
		    require_terms       = excluded.require_terms,
		    allow_international = excluded.allow_international`,
		chatID, query, nullInt(spec.TargetCents), nullInt(spec.MinCents), nullInt(spec.MaxCents),
		strings.Join(spec.Exclude, "|"), strings.Join(spec.Require, "|"),
		boolInt(spec.AllowInternational), now)
	if err != nil {
		return nil, fmt.Errorf("store: create watch: %w", err)
	}
	return s.WatchByQuery(ctx, chatID, query)
}

// WatchByQuery looks a watch up by its exact query text.
func (s *Store) WatchByQuery(ctx context.Context, chatID int64, query string) (*Watch, error) {
	return s.scanWatch(s.db.QueryRowContext(ctx,
		selectWatch+` WHERE chat_id = ? AND query = ?`, chatID, query))
}

// Watch returns one watch by id.
func (s *Store) Watch(ctx context.Context, id int64) (*Watch, error) {
	return s.scanWatch(s.db.QueryRowContext(ctx, selectWatch+` WHERE id = ?`, id))
}

// Watches lists a chat's watches, newest last. Inactive ones are included so
// the manage UI can show and resume them.
func (s *Store) Watches(ctx context.Context, chatID int64) ([]Watch, error) {
	rows, err := s.db.QueryContext(ctx, selectWatch+` WHERE chat_id = ? ORDER BY id`, chatID)
	if err != nil {
		return nil, fmt.Errorf("store: list watches: %w", err)
	}
	defer rows.Close()
	return collectWatches(rows)
}

// ActiveWatches lists every active watch across all chats, for the scheduler.
func (s *Store) ActiveWatches(ctx context.Context) ([]Watch, error) {
	rows, err := s.db.QueryContext(ctx, selectWatch+` WHERE active = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list active watches: %w", err)
	}
	defer rows.Close()
	return collectWatches(rows)
}

// SetWatchActive pauses or resumes a watch.
func (s *Store) SetWatchActive(ctx context.Context, id int64, active bool) error {
	return s.exec(ctx, "set watch active", `UPDATE watches SET active = ? WHERE id = ?`, boolInt(active), id)
}

// SetWatchBounds sets or clears (0) the price range a watch tracks.
func (s *Store) SetWatchBounds(ctx context.Context, id, minCents, maxCents int64) error {
	return s.exec(ctx, "set watch bounds",
		`UPDATE watches SET min_cents = ?, max_cents = ? WHERE id = ?`,
		nullInt(minCents), nullInt(maxCents), id)
}

// RenameWatch changes a watch's search query, keeping its id, filters and the
// price history of everything it already tracks.
func (s *Store) RenameWatch(ctx context.Context, id int64, query string) error {
	query = strings.Join(strings.Fields(query), " ")
	if query == "" {
		return errors.New("store: empty query")
	}
	return s.exec(ctx, "rename watch", `UPDATE watches SET query = ? WHERE id = ?`, query, id)
}

// SetNotifiedBest records the best price the user has been told about.
func (s *Store) SetNotifiedBest(ctx context.Context, id, cents int64) error {
	return s.exec(ctx, "set notified best",
		`UPDATE watches SET notified_best_cents = ? WHERE id = ?`, cents, id)
}

// SetWatchRequire replaces the terms a watch's listings must contain.
func (s *Store) SetWatchRequire(ctx context.Context, id int64, terms []string) error {
	return s.exec(ctx, "set watch require",
		`UPDATE watches SET require_terms = ? WHERE id = ?`, strings.Join(terms, "|"), id)
}

// SetWatchInternational chooses whether a watch keeps cross-border listings.
func (s *Store) SetWatchInternational(ctx context.Context, id int64, allow bool) error {
	return s.exec(ctx, "set watch international",
		`UPDATE watches SET allow_international = ? WHERE id = ?`, boolInt(allow), id)
}

// SetWatchExclude replaces a watch's exclusion terms.
func (s *Store) SetWatchExclude(ctx context.Context, id int64, terms []string) error {
	return s.exec(ctx, "set watch exclude",
		`UPDATE watches SET exclude_terms = ? WHERE id = ?`, strings.Join(terms, "|"), id)
}

// SetWatchTarget sets or clears (0) a watch's target price.
func (s *Store) SetWatchTarget(ctx context.Context, id, targetCents int64) error {
	return s.exec(ctx, "set watch target", `UPDATE watches SET target_cents = ? WHERE id = ?`, nullInt(targetCents), id)
}

// MarkWatchScanned records that a scan just finished.
func (s *Store) MarkWatchScanned(ctx context.Context, id int64, at time.Time) error {
	return s.exec(ctx, "mark scanned", `UPDATE watches SET last_scan_at = ? WHERE id = ?`, formatTime(at), id)
}

// DeleteWatch removes a watch. Its products stay: another watch may share them,
// and the price history is worth keeping either way.
func (s *Store) DeleteWatch(ctx context.Context, id int64) error {
	return s.exec(ctx, "delete watch", `DELETE FROM watches WHERE id = ?`, id)
}

const selectWatch = `
	SELECT id, chat_id, query, COALESCE(target_cents, 0), COALESCE(min_cents, 0), COALESCE(max_cents, 0),
	       COALESCE(exclude_terms, ''), COALESCE(require_terms, ''), COALESCE(allow_international, 0),
	       COALESCE(notified_best_cents, 0), active, created_at,
	       COALESCE(last_scan_at, '')
	FROM watches`

type rowScanner interface{ Scan(dest ...any) error }

func (s *Store) scanWatch(row rowScanner) (*Watch, error) {
	w, err := scanWatchInto(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan watch: %w", err)
	}
	return &w, nil
}

func scanWatchInto(row rowScanner) (Watch, error) {
	var (
		w        Watch
		exclude  string
		require  string
		intl     int
		active   int
		created  string
		lastScan string
	)
	if err := row.Scan(&w.ID, &w.ChatID, &w.Query, &w.TargetCents, &w.MinCents, &w.MaxCents,
		&exclude, &require, &intl, &w.NotifiedBestCents, &active, &created, &lastScan); err != nil {
		return Watch{}, err
	}
	w.Exclude = splitTerms(exclude)
	w.Require = splitTerms(require)
	w.AllowInternational = intl != 0
	w.Active = active != 0
	w.CreatedAt = parseTime(created)
	w.LastScanAt = parseTime(lastScan)
	return w, nil
}

func collectWatches(rows *sql.Rows) ([]Watch, error) {
	var out []Watch
	for rows.Next() {
		w, err := scanWatchInto(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan watch: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// --- products and prices ---

// UpsertProduct inserts or refreshes a product and returns its id. Titles and
// prices drift; the (source, external_id) pair is what stays put.
func (s *Store) UpsertProduct(ctx context.Context, p Product) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO products (source, external_id, title, url, image_url, seller, international)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, external_id) DO UPDATE SET
		    title         = excluded.title,
		    url           = excluded.url,
		    image_url     = excluded.image_url,
		    seller        = excluded.seller,
		    international = excluded.international
		RETURNING id`,
		p.Source, p.ExternalID, p.Title, p.URL, p.ImageURL, p.Seller, boolInt(p.International)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upsert product %s/%s: %w", p.Source, p.ExternalID, err)
	}
	return id, nil
}

// LinkWatchProduct records that a watch's query surfaced this product.
func (s *Store) LinkWatchProduct(ctx context.Context, watchID, productID int64, at time.Time) error {
	ts := formatTime(at)
	return s.exec(ctx, "link watch product", `
		INSERT INTO watch_products (watch_id, product_id, first_seen, last_seen)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (watch_id, product_id) DO UPDATE SET last_seen = excluded.last_seen`,
		watchID, productID, ts, ts)
}

// UnlinkWatchProduct stops a watch following a product. The product and its
// price history stay: another watch may share it, and the history is worth
// keeping if it comes back.
func (s *Store) UnlinkWatchProduct(ctx context.Context, watchID, productID int64) error {
	return s.exec(ctx, "unlink watch product",
		`DELETE FROM watch_products WHERE watch_id = ? AND product_id = ?`, watchID, productID)
}

// LatestPrice returns the most recent observation for a product.
func (s *Store) LatestPrice(ctx context.Context, productID int64) (*PricePoint, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT product_id, price_cents, list_price_cents, site_flags, seen_at
		FROM price_points WHERE product_id = ?
		ORDER BY seen_at DESC, id DESC LIMIT 1`, productID)

	p, err := scanPricePoint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest price: %w", err)
	}
	return &p, nil
}

// AddPricePoint appends an observation.
func (s *Store) AddPricePoint(ctx context.Context, p PricePoint) error {
	return s.exec(ctx, "add price point", `
		INSERT INTO price_points (product_id, price_cents, list_price_cents, site_flags, seen_at)
		VALUES (?, ?, ?, ?, ?)`,
		p.ProductID, p.PriceCents, p.ListPriceCents, strings.Join(p.SiteFlags, "|"), formatTime(p.SeenAt))
}

// PriceHistory returns a product's observations since a cutoff, oldest first.
// Pass a zero time for the whole history.
func (s *Store) PriceHistory(ctx context.Context, productID int64, since time.Time) ([]PricePoint, error) {
	cutoff := ""
	if !since.IsZero() {
		cutoff = formatTime(since)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT product_id, price_cents, list_price_cents, site_flags, seen_at
		FROM price_points
		WHERE product_id = ? AND (? = '' OR seen_at >= ?)
		ORDER BY seen_at, id`, productID, cutoff, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: price history: %w", err)
	}
	defer rows.Close()

	var out []PricePoint
	for rows.Next() {
		p, err := scanPricePoint(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan price point: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPricePoint(row rowScanner) (PricePoint, error) {
	var (
		p     PricePoint
		flags string
		seen  string
	)
	if err := row.Scan(&p.ProductID, &p.PriceCents, &p.ListPriceCents, &flags, &seen); err != nil {
		return PricePoint{}, err
	}
	if flags != "" {
		p.SiteFlags = strings.Split(flags, "|")
	}
	p.SeenAt = parseTime(seen)
	return p, nil
}

// --- alerts ---

// LastAlert returns the most recent alert of a kind for a product.
func (s *Store) LastAlert(ctx context.Context, productID int64, kind string) (time.Time, int64, error) {
	var (
		firedAt string
		cents   int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT fired_at, price_cents FROM alerts
		WHERE product_id = ? AND kind = ?
		ORDER BY fired_at DESC, id DESC LIMIT 1`, productID, kind).Scan(&firedAt, &cents)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, 0, ErrNotFound
	}
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("store: last alert: %w", err)
	}
	return parseTime(firedAt), cents, nil
}

// RecordAlert notes that an alert was sent, so it is not repeated.
func (s *Store) RecordAlert(ctx context.Context, watchID, productID int64, kind string, priceCents int64, at time.Time) error {
	return s.exec(ctx, "record alert", `
		INSERT INTO alerts (watch_id, product_id, kind, price_cents, fired_at)
		VALUES (?, ?, ?, ?, ?)`, watchID, productID, kind, priceCents, formatTime(at))
}

// --- reporting ---

// WatchOffer is a product a watch is tracking, with its latest price.
type WatchOffer struct {
	Product
	PriceCents     int64
	ListPriceCents int64
	SiteFlags      []string
	SeenAt         time.Time
	LowCents       int64 // lowest price recorded in the trailing window
}

// WatchOffers returns a watch's products ranked cheapest first, each with its
// latest price and its low over the trailing window.
func (s *Store) WatchOffers(ctx context.Context, watchID int64, window time.Duration, limit int) ([]WatchOffer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.source, p.external_id, p.title, p.url, p.image_url, p.seller, p.international,
		       lp.price_cents, lp.list_price_cents, lp.site_flags, lp.seen_at,
		       COALESCE((SELECT MIN(price_cents) FROM price_points
		                 WHERE product_id = p.id AND seen_at >= ?), lp.price_cents)
		FROM watch_products wp
		JOIN products p ON p.id = wp.product_id
		JOIN price_points lp ON lp.id = (
		    SELECT id FROM price_points WHERE product_id = p.id
		    ORDER BY seen_at DESC, id DESC LIMIT 1)
		WHERE wp.watch_id = ?
		ORDER BY lp.price_cents
		LIMIT ?`,
		formatTime(time.Now().Add(-window)), watchID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: watch offers: %w", err)
	}
	defer rows.Close()

	var out []WatchOffer
	for rows.Next() {
		var (
			o     WatchOffer
			intl  int
			flags string
			seen  string
		)
		if err := rows.Scan(&o.ID, &o.Source, &o.ExternalID, &o.Title, &o.URL, &o.ImageURL, &o.Seller, &intl,
			&o.PriceCents, &o.ListPriceCents, &flags, &seen, &o.LowCents); err != nil {
			return nil, fmt.Errorf("store: scan watch offer: %w", err)
		}
		o.International = intl != 0
		if flags != "" {
			o.SiteFlags = strings.Split(flags, "|")
		}
		o.SeenAt = parseTime(seen)
		out = append(out, o)
	}
	return out, rows.Err()
}

// WatchStats summarizes a watch for the manage UI.
type WatchStats struct {
	Products  int
	BestCents int64
	BestID    int64
	LowCents  int64
}

// Stats returns a watch's headline numbers over the trailing window.
func (s *Store) Stats(ctx context.Context, watchID int64, window time.Duration) (WatchStats, error) {
	offers, err := s.WatchOffers(ctx, watchID, window, 1000)
	if err != nil {
		return WatchStats{}, err
	}

	st := WatchStats{Products: len(offers)}
	for i, o := range offers {
		if i == 0 {
			st.BestCents, st.BestID, st.LowCents = o.PriceCents, o.ID, o.LowCents
			continue
		}
		if o.LowCents > 0 && o.LowCents < st.LowCents {
			st.LowCents = o.LowCents
		}
	}
	return st, nil
}

func (s *Store) exec(ctx context.Context, what, query string, args ...any) error {
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	return nil
}

// Times are stored as RFC3339 in UTC so string ordering matches chronological
// ordering, which is what the range queries above rely on.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func splitTerms(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "|")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
