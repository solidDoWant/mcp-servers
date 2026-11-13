# electronic-components changelog

## 0.3.0

- The cache and budgets can live in a shared Redis-protocol server (`CACHE_URL_FILE`, `CACHE_KEY_PREFIX`), so they survive restarts, daily counts are exact, and replicas share them. Memory stays the default.
- When a source fails, a recent answer (up to `CACHE_STALE_FOR`, a day by default) is used instead, marked stale with its age and the failure.
- An answer with no offers is cached for 10 minutes, not an hour.
- `from_cache` in `search_parts` (and now `get_part`) lists each cached answer's source, age and staleness, instead of only source names. `list_sources` reports `cache_backend` and `stale_for` instead of `cache_entries`.

## 0.2.0

- A part number made by several companies, such as a generic AMS1117-3.3, now lists every maker under `manufacturers` instead of naming the first one as `manufacturer`, and the best offer says whose part it is. Makers' names are compared ignoring case and punctuation.

## 0.1.0

First release, renamed from `parts` (released once as `parts/v0.1.0`), whose name said too little. Its cache settings are now `CACHE_TTL` and `CACHE_SIZE`.

- Search DigiKey, Mouser and JLCPCB's parts library (through jlcsearch) at once, with offers grouped by manufacturer part number and the cheapest offer with enough stock at a quantity.
- Look up one part by manufacturer or distributor part number, with specifications.
- Each source is enabled only when its credentials are set; jlcsearch needs none.
- jlcsearch is tried up to three times after a server error.
- Answers are cached, and each source has a rate and a daily budget below the distributor's, so one shared instance can serve everyone.
