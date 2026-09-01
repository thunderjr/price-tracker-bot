# price-tracker-bot

Telegram bot that tracks prices, promotions and offers for free-text queries
("ps5", "LEGO Millennium Falcon") on **Mercado Livre** and **Amazon Brasil**.

```
/track ps5 | max 3500     start tracking, with an optional target price
/manage                   inline-keyboard manager (scan, target, pause, remove)
/list                     plain text snapshot
/help
```

You get a message when a price falls well below its own 30-day median, hits the
lowest value ever recorded, crosses your target, or when the listing itself
starts advertising a promotion.

## Quick start

```sh
cp .env.example .env      # TELEGRAM_BOT_TOKEN from @BotFather
                          # ALLOWED_CHAT_IDS from @userinfobot
make up
make logs
```

Then message the bot: `/track ps5`.

## How the two sources work, and why they differ

**Amazon BR** is scraped over plain HTTP with `net/http` + goquery. It serves
complete server-rendered search results to an ordinary client with a desktop
User-Agent — no browser needed, ~1 second per query.

**Mercado Livre** needs a real browser, and this is the load-bearing constraint
of the whole project:

| approach | result |
|---|---|
| `api.mercadolibre.com/sites/MLB/search` | 403 `forbidden`, even for trivial queries |
| `/products/search`, `/items/{id}` | 403 `PA_UNAUTHORIZED_RESULT_FROM_POLICIES` |
| `curl` with full browser headers + cookie jar | redirected to the `suspicious-traffic` wall |
| Chrome / Chromium `--headless=new` over CDP | *"Hubo un error accediendo a esta pagina"* |
| **Chromium headful over CDP** | ✅ full results |

The deciding factor is **headful vs headless**, not which Chromium build. That
is why the container installs `chromium` plus `xvfb` and the entrypoint starts a
virtual display: the browser genuinely renders, into a screen nobody looks at.
Chromium is packaged for arm64, so this works on Apple Silicon without Rosetta.

`internal/browser` keeps **one** Chromium alive for the process lifetime, with
its profile on the `/data` volume so cookies and site reputation survive a
restart. A failed run tears the browser down and relaunches it once.

Two details there are load-bearing, and both caused outages before they were
fixed:

- **Tabs must come from the browser context, not the allocator.**
  `chromedp.NewContext(allocCtx)` starts a *whole new Chromium*; only
  `chromedp.NewContext(browserCtx)` opens a tab. Getting this wrong launches a
  browser per search, and two browsers on one profile deadlock over its lock.
- **The profile lock has to be cleared on startup.** Chromium's `SingletonLock`
  records the hostname that took it (`SingletonLock -> 0cd9aae89cd6-47`). Every
  container restart brings a new hostname, so Chromium decides another machine
  owns the profile and refuses to start — permanently. Any unclean shutdown
  would otherwise take Mercado Livre out for good.

`internal/browser` has live tests for both: one asserts three searches
share a single Chromium **pid**, the other plants a foreign-hostname lock and
requires the browser to come up anyway.

## Smoke test

Selector rot and anti-bot changes are the two things that will break this. One
command checks both against the live sites:

```sh
make smoke
```

It must print at least 10 offers per source:

```
meli: 48 offers in 2.928s
  MLB57081243  R$ 4.047,00 -12%  Console Playstation®5 Slim Digital ...
amazon: 59 offers in 956ms
  B0FPGF9J2J   R$ 4.184,06 - 7%  PlayStation®5 Slim Digital 825GB ...
probe ok
```

Run it after every base-image or dependency bump. `ptb probe chrome <url>` is
the finer-grained tool: it reports the page title, size and whether the content
looks like an anti-bot interstitial, and dumps the HTML when `PTB_DUMP` is set.

`make integration` goes further: a live scan of both marketplaces into a real
SQLite database, then a backdated history to prove `drop_vs_median` fires and
the cooldown then suppresses it. It cross-compiles the test binary and mounts
it into the container, because the runtime image carries no Go toolchain.

A note if you extend it: Amazon answers roughly four searches in a minute with a
captcha (a 2 KB page titled `&nbsp;`). That is a sane rate limit, not a bug —
keep live tests down to one Amazon request.

The two failure modes are reported separately, because they need opposite
responses:

| error | meaning | response |
|---|---|---|
| `ErrThrottled` | captcha, 503/429, or a sub-50 KB unrecognized page | back off and retry (5s, 20s, 45s) |
| `ErrBlocked` | a full-size page the parser no longer understands | the site changed; fix the parser |

`ErrThrottled` satisfies `errors.Is(err, ErrBlocked)`, so callers that only care
"this source gave nothing" are unaffected. The live test skips a throttled
source and fails on a blocked one, so a redesign can never hide behind retries.

## What gets tracked, and what gets dropped

A raw "ps5" search on Amazon BR returns 59 listings: 23 consoles and 36 games,
stands, coolers and controllers. Tracking all of it is useless, so
`internal/relevance` filters — in two layers, because the two halves of the
problem are genuinely different.

**Automatic: accessories, look-alikes, duplicates.** Titles that name what they
are *for* (`Para Console Ps5 Slim`, `Kit De Iluminação Para Lego`), that carry
an accessory noun (suporte, capa, cooler, ventilador, headset, minifigura), or
that advertise a clone (`Estilo lego`, `Blocos Compatível`). Measured against
**218 hand-labelled real listings** across four fixtures: 40% of the junk caught
on Amazon "ps5" (the rest are games, which need a price bound), 88% on "lego
millennium falcon", 91% on "LEGO Olivia Rodrigo".

Four real listings are knowingly lost, each named with its reason in
`falseDropBudget` — two Spanish-titled ("Halcon Milenario") and two whose title
omits half the query. All four are duplicates of sets that stay covered by other
listings, so no price history is lost. Any *other* real listing being dropped
fails the test.

**Automatic: cross-border listings.** Their advertised price excludes Brazilian
import tax, so it is not what you pay — and being artificially low, an import
wins every "cheapest offer" comparison and hides the real best price. Mercado
Livre flags these structurally (`.poly-component__cbt`, with the origin);
Amazon only says so in the title. `| internacional` opts back in.

**Automatic: query coverage.** A marketplace answers a specific query with
loosely related stock once it runs out of real matches — "LEGO Olivia Rodrigo"
comes back with Minecraft and Disney sets sharing only the word "lego".
Titles must carry half the query's words. This stands down whenever the query
is worded differently from the listings ("ps5" against titles reading
"PlayStation 5"), because there it cannot tell noise from an alias.

**Explicit: price range and terms.** A PS5 game and a PS5 console differ only
in price, not in any safe title signal. And when coverage stands down, only the
user knows whether the leftovers are noise:

```
/track ps5 | min 3000
/track ps5 | min 3000 | alvo 4200
/track lego millennium falcon | -iluminacao -minifigura
/track lego olivia rodrigo | +rodrigo +lego
```

With `min 3000`, all 36 junk listings go and all 23 consoles stay.

### Why there is no automatic price filter

It is the obvious idea and it does not work. `lego millennium falcon`
legitimately returns the 74-piece polybag at **R$84** and the 7541-piece UCS set
at **R$14.999** — every price-anchored rule tried (median, top-N median, 75th
percentile, largest-gap) threw away one end or the other. Worse, on Amazon the
junk *outnumbers* the signal 36 to 23, so the median sits at R$300 and a
median-based filter keeps the games and drops the consoles.

Instead the bot **offers** the cut. After the first scan, if the prices fall
into two groups separated by a 3× gap with enough listings above it, it asks:

> 💡 Os preços se dividem em dois grupos. Rastrear só o grupo acima de R$ 3.765?
> `[ ✂️ Sim, filtrar ]  [ Manter tudo ]`

On the Amazon "ps5" fixture that suggestion cuts 22 junk listings and loses
zero real ones. On the LEGO fixture it stays silent, which is the whole point.

## Alert rules

| rule | fires when | confidence |
|---|---|---|
| `drop_vs_median` | price is `DROP_THRESHOLD` below its own 30-day median (≥5 points) | high |
| `new_low` | price matches or beats the lowest ever recorded | high |
| `target` | price crosses the target you set | high |
| `site_flag` | the listing starts advertising a promotion | **low** — flagged as such in the message |

The median, not the mean, is the baseline: one spike or one flash sale in the
history must not manufacture a fake drop. `site_flag` is reported as low
confidence on purpose, because marketplace "de/por" reference prices are
routinely inflated.

The same alert will not repeat for 24h unless the price falls another 5%.

## Configuration

| variable | default | meaning |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | — | required |
| `ALLOWED_CHAT_IDS` | — | required; comma-separated. Without it the bot answers nobody |
| `SCAN_INTERVAL` | `3h` | how often every active watch is rescanned |
| `DROP_THRESHOLD` | `0.10` | how far below the median counts as a drop |
| `DATA_DIR` | `/data` | holds `ptb.db` and `chrome-profile/` |
| `CHROME_PATH` | `/usr/bin/chromium` | |
| `LOG_LEVEL` | `info` | |

Keep `SCAN_INTERVAL` generous. This is a personal tracker; low request volume is
what keeps both scrapers working.

## Layout

```
cmd/ptb/              serve + probe
internal/browser/     the one headful Chromium, and its restart logic
internal/source/      Source interface, Offer, BRL parsing
  amazon/             net/http + goquery  (+ a saved search page fixture)
  meli/               chromedp, one in-page extraction call
internal/store/       SQLite (modernc.org/sqlite, no cgo)
internal/tracker/     scan loop, alert rules, scheduler
internal/telegram/    commands, inline-keyboard manager, alert formatting
```

## Caveats

- Both sources are scraped. Selectors will break eventually; the Amazon fixture
  test tells you it was Amazon, and `make smoke` tells you it was Mercado Livre.
- Products are **not** matched across marketplaces. Fuzzy-matching an ASIN to an
  MLB id by title has a poor accuracy ceiling, so price history is per listing
  and a watch simply groups whatever each source returned.
- If Mercado Livre ever fingerprints deeper than headless detection, the fix is a
  stealth browser (Camoufox, Patchright) or a paid proxy — not a tweak here.
