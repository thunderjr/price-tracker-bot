PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS watches (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id      INTEGER NOT NULL,
    query        TEXT    NOT NULL,
    target_cents INTEGER,
    min_cents    INTEGER,
    max_cents    INTEGER,
    exclude_terms TEXT NOT NULL DEFAULT '',
    require_terms TEXT NOT NULL DEFAULT '',
    allow_international INTEGER NOT NULL DEFAULT 0,
    notified_best_cents INTEGER NOT NULL DEFAULT 0,
    active       INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT    NOT NULL,
    last_scan_at TEXT,
    UNIQUE (chat_id, query)
);

CREATE TABLE IF NOT EXISTS products (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    source      TEXT NOT NULL,
    external_id TEXT NOT NULL,
    title       TEXT NOT NULL,
    url         TEXT NOT NULL,
    image_url   TEXT NOT NULL DEFAULT '',
    seller      TEXT NOT NULL DEFAULT '',
    international INTEGER NOT NULL DEFAULT 0,
    UNIQUE (source, external_id)
);

CREATE TABLE IF NOT EXISTS watch_products (
    watch_id   INTEGER NOT NULL REFERENCES watches(id)  ON DELETE CASCADE,
    product_id INTEGER NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    first_seen TEXT    NOT NULL,
    last_seen  TEXT    NOT NULL,
    PRIMARY KEY (watch_id, product_id)
);

CREATE TABLE IF NOT EXISTS price_points (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id       INTEGER NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    price_cents      INTEGER NOT NULL,
    list_price_cents INTEGER NOT NULL DEFAULT 0,
    site_flags       TEXT    NOT NULL DEFAULT '',
    seen_at          TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_price_points_product
    ON price_points (product_id, seen_at DESC);

CREATE TABLE IF NOT EXISTS alerts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    watch_id    INTEGER NOT NULL REFERENCES watches(id)  ON DELETE CASCADE,
    product_id  INTEGER NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL,
    price_cents INTEGER NOT NULL,
    fired_at    TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_alerts_recent
    ON alerts (product_id, kind, fired_at DESC);
