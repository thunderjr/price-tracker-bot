# price-tracker-bot

Telegram bot that tracks prices, promotions and offers for free-text queries
("ps5", "LEGO Millennium Falcon") on **Mercado Livre**, **Amazon Brasil** and
**KaBuM!**.

```
/track ps5 | max 3500     start tracking, with an optional target price
/manage                   inline-keyboard manager (scan, target, pause, remove,
                          à vista / parcelado)
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

## How the three sources work, and why they differ

**Amazon BR** is scraped over plain HTTP with `net/http` + goquery. It serves
complete server-rendered search results to an ordinary client with a desktop
User-Agent — no browser needed, ~1 second per query.

**KaBuM!** also needs no browser, and gives up something better than markup: its
search page is a Next.js app whose `__NEXT_DATA__` script carries the entire
result set as JSON, so offers are read from structured fields instead of CSS
selectors. Two things about it are worth knowing before you touch that source —
both are covered in *KaBuM! quirks* below.

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

## KaBuM! quirks

**Its catalogue API is not usable for this.** There is a public endpoint at
`servicespub.prod.api.aws.grupokabum.com.br/catalog/v2/products?query=…`, and it
only answers for queries that resolve to a category. `playstation 5` works;
`lego millennium falcon` comes back `CATALOG_NOT_MATCH` even though the site
search finds real LEGO sets for it. This bot tracks arbitrary free-text
queries, so the search page is the only endpoint that can serve them.

**It never reports an empty search.** A query matching nothing is padded with
unrelated recommendations rather than returning zero results — `zzqqxxnaoexiste`
still claims 14,400 items and fills a page with phone screen protectors and
desks. So there is no such thing here as a legitimately empty result page: a
missing payload means blocked or redesigned, never "no results". The relevance
filter is what keeps that padding out of a watch.

**Its prices arrive as 32-bit floats.** The same figure shows up on one page as
both `127.96` and `127.95999908447266`, and R$ 1.943,33 as `1943.3299560546875`.
Money in this codebase is `int64` cents and no float ever touches it, so the
JSON number is kept as its literal text (`json.Number`) and rounded half-up on
the third decimal. `ParseBRL` cannot do this job: it reads `.` as a pt-BR
thousands separator once there are more than two decimals, which turns that
first figure into R$ 127.959.999.084.472,66 — large enough, incidentally, to
overflow the tolerance arithmetic that would otherwise have caught it.

**Its instalment line goes stale when a seller re-prices.** Caught live during
this source's first container run: two marketplace listings had moved to
R$ 6.509,90 and R$ 7.079,90 on the card while `maxInstallment` still read
"10x de R$ 420,94" and "10x de R$ 482,41". Both halves of that payload are
traps. Financing is never cheaper than the card price, so a plan totalling less
than it is stale rather than a bargain — and in `parcelado` mode the watch ranks
on the plan total, so publishing it would have put the dearest console on the
page at the top of the list at a phantom R$ 4.209,40. A plan that does not total
the card price is therefore discarded, and a plan totalling *more* is kept and
labelled `com juros`, because that direction really is interest.

The same staleness is why `oldPrice` is judged against the card price directly
rather than against the plan total the way Amazon's struck figure is: `oldPrice`
is a former price only when it sits *above* the card price. Equal to it — the
usual case — it is the card price restated, and calling that a markdown turns
the Pix discount into a permanent 5–7% "discount" on nearly every listing.

**Its product descriptions break the JSON.** They are HTML pasted in with the
newlines intact, and a literal newline inside a JSON string is invalid —
`encoding/json` rejects the whole document, which would drop every offer on the
page over a field the source never even reads. Bytes below `0x20` are never
legal there, so they are blanked before decoding.

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

**Automatic: listings that went away.** A watch stops following a listing once
three consecutive scans of a source that *answered* came back without it.
Absence during a block proves nothing, and the tail of a search page shifts
between requests — but a delisted offer left in place goes on being the watch's
best price for months.

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

## Advertised discounts are checked, not believed

Both marketplaces strike out a figure beside the cash price, and on most
listings it is simply the same item paid in instalments. Amazon shows

> **R$ 4.184,06** ~~R$ 4.499,00~~ à vista no Pix ou NuPay ou em até 10x de R$ 449,90

and ten times 449,90 is exactly 4.499,00. Mercado Livre does the same with
"ou R$ 4.599,90 em 10x R$ 459,99". Read as a former price, that gap becomes a
5–8% discount that never expires, on nearly every listing, burying the real
ones.

So the installment plan is parsed and the reference price checked against it: a
figure within 1.5% of `count × each` is the financing total, not a markdown.
On the Amazon "ps5" fixture that removes 17 of 26 struck figures and keeps 9 —
Spider-Man 2 at −55%, ASTRO BOT at −42%, Resident Evil at −20%.

The financing offer is kept and shown, since it is what the instalments cost:

```
R$ 4.184,06 · Amazon
ou 10x R$ 449,90 (total R$ 4.499,00)
PlayStation®5 Slim Digital 825GB
```

Every way to pay is listed, interest-bearing plans included. Mercado Livre
prints "sem juros" only when a plan really is free and writes nothing at all
otherwise, so a plan whose instalments add up to more than the cash price is
labelled `com juros` from its own arithmetic — silence would read as free.

KaBuM! needs the opposite reading, and it is the same offer that shows why. It
never writes "juros" anywhere on a search page, but it publishes both figures as
fields: a card price and a lower cash price for Pix. Its plans total the card
price to the cent, so the plan is free and the gap down to the cash price is a
Pix discount, not financing cost. Comparing against the cash price the way
Mercado Livre demands would label all 60 plans on a "playstation 5" page
`com juros` — and contradict Amazon, which sells that console at the very same
three numbers (R$ 4.184 cash, R$ 4.499 on the card, 10x R$ 449,90) and prints
"sem juros" outright. So a source that states the wording is believed, a source
that publishes a reference price is measured against it, and only Mercado
Livre's silence is read as interest.

## À vista or parcelado

The two prices do not rank the same listings. Mercado Livre's own
"playstation 5 slim" results, on one scan:

| cash | plan | financed |
|---|---|---|
| R$ 4.599,00 | 12x R$ 442,00 com juros | R$ 5.304,00 |
| R$ 4.742,00 | 10x R$ 509,00 sem juros | R$ 5.090,00 |
| R$ 4.849,00 | 10x R$ 504,00 sem juros | R$ 5.040,00 |

The cheapest console to pay cash for is the **most** expensive of the three to
finance, and the cheapest to finance is the priciest in cash. Ranking on the
wrong figure picks the wrong console.

So each watch chooses which figure it shops on — 💳 in `/manage`, or
`ptb watch -mode parcelado <id>`. The mode decides the ranking, the 30-day low,
the target, and every alert, and the message says which it used:

```
💳 por total parcelado

R$ 5.038,80 · Amazon
12x R$ 419,90 com juros (total R$ 5.038,80)
ou R$ 4.100,00 à vista
PlayStation 5 Slim
```

Offers are ordered by that total, ascending, and the total is printed beside
the instalments that produce it — the per-instalment amount alone says nothing
about what a plan costs, and the longest plan is regularly the dearest.

A listing with no plan falls back to its cash price — paying cash is always
available, and dropping it would hide a real offer. Switching mode clears the
watch's notification baseline, so the change itself is never reported as a
price move.

## Alert rules

| rule | fires when | confidence |
|---|---|---|
| `best_drop` / `best_rise` | the watch's cheapest offer moves more than `BEST_MOVE_THRESHOLD` since you were last told | high |
| `drop_vs_median` | price is `DROP_THRESHOLD` below its own 30-day median (≥5 points) | high |
| `new_low` | price beats the lowest ever recorded, or comes back down to it | high |
| `target` | price crosses the target you set | high |
| `site_flag` | the listing starts advertising a promotion | **low** — flagged as such in the message |

`best_drop` and `best_rise` are the running commentary: they say nothing about
whether the price is *good*, only that it moved, so you hear about movement
without having to ask. They are measured against the figure you were last
notified of rather than the previous scan, so a slow slide is announced once it
adds up, and a price flapping between two values does not notify twice.

The four rules below them require 12 hours of history and at least 3
observations. Without that, a watch's second scan declares half the catalogue a
record low — which is exactly what happened once, as 56 notifications.

`new_low` needs the price to have moved: an unchanged price is trivially its
own lowest ever, and with a daily heartbeat point in the history a flat watch
would announce a fresh record every time the cooldown lapsed.

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
| `BEST_MOVE_THRESHOLD` | `0.01` | how far a watch's best price must move to notify |
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
