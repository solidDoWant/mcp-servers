# electronic-components

Search electronic component distributors and compare offers: [DigiKey](https://www.digikey.com), [Mouser](https://www.mouser.com), and JLCPCB's parts library (the parts JLCPCB stocks for PCB assembly), through [jlcsearch](https://github.com/tscircuit/jlcsearch).

## Tools

| Tool | What it does |
|---|---|
| `search_parts` | Search every enabled source at once by keyword or part number. Offers for the same manufacturer part number are grouped, with the cheapest offer at a quantity that has enough stock. |
| `get_part` | Look up one part by manufacturer part number or a distributor's own number (DigiKey `296-1234-ND`, Mouser `511-LM317T`, LCSC `C8734`), with specifications. |
| `list_sources` | Which sources are enabled (and why not), and how much of each daily budget is used. |

A source that fails, or has spent its budget, is listed under `errors` while the others still answer. A call fails only when every source it asked fails.

For JLCPCB assembly, *basic* parts carry no per-part setup fee and *extended* parts do. Each JLCPCB offer says which it is, and `jlc_basic_only` finds basic parts only.

## Sources

| Source | Needs | Free limits |
|---|---|---|
| `digikey` | A DigiKey API app's client ID and secret ([developer portal](https://developer.digikey.com/), Product Information v4) | 120 calls a minute, 1000 a day |
| `mouser` | A Mouser **Search** API key ([apply](https://www.mouser.com/api-search/)). It can't reach Mouser's cart or order APIs, which use a different key. | 30 calls a minute, 1000 a day |
| `jlcpcb` | Nothing. jlcsearch is free and keyless. | None published |

Each source is enabled only when its credentials are set; jlcsearch is enabled unless `JLCSEARCH_DISABLED=true`. So the server works with jlcsearch alone, and gains DigiKey and Mouser when their keys are added.

### Budgets and the cache

The server is meant to run as one shared instance, so everyone shares one day's calls. To make them last:

- Answers are cached in memory (1 hour by default) and identical calls in flight share one request. A cache hit costs nothing. `search_parts` lists the sources it answered from the cache.
- Each source has a rate and a daily budget below the distributor's own: DigiKey 100 a minute and 950 a day, Mouser 25 a minute and 950 a day. A call that would wait more than 10 seconds for the rate, or comes after the day's budget is spent, fails for that source only. Budgets reset at 00:00 UTC.

The cache and the day's counts are lost on restart. That is the only state the server keeps; any replica can answer any request, but each replica has its own cache and budget, so run one.

### Keys stay out of output and logs

Mouser takes its key as a URL parameter, and Go's HTTP errors quote the URL. The server removes the URL from every upstream error and never logs request URLs, so the key can't reach tool output or logs. A test checks this.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `MCP_LISTEN_ADDR` | `:8080` | Where to serve MCP (`/mcp`) and the health check (`/healthz`). |
| `DIGIKEY_CLIENT_ID_FILE`, `DIGIKEY_CLIENT_SECRET_FILE` | none | Files holding DigiKey's client ID and secret. Both are needed. A missing file leaves DigiKey off. |
| `MOUSER_API_KEY_FILE` | none | File holding a Mouser Search API key. |
| `JLCSEARCH_DISABLED` | `false` | Turn jlcsearch off. |
| `DIGIKEY_LOCALE_SITE`, `DIGIKEY_LOCALE_CURRENCY` | `US`, `USD` | DigiKey's prices and stock for this site and currency. |
| `CACHE_TTL`, `CACHE_SIZE` | `1h`, `1000` | How long answers are reused, and how many are kept. A search keeps up to about 50 KB, so 1000 entries stay under about 50 MB. |
| `DIGIKEY_DAILY_LIMIT`, `DIGIKEY_PER_MINUTE`, `MOUSER_DAILY_LIMIT`, `MOUSER_PER_MINUTE` | see above | Budgets. A daily limit of `0` means none. |
| `DIGIKEY_API_URL`, `MOUSER_API_URL`, `JLCSEARCH_URL` | the real services | Upstream addresses. Tests point them at a fake. |

Keys are read at start-up; restart the server after changing them.

The server needs outbound HTTPS to `api.digikey.com`, `api.mouser.com` and `jlcsearch.tscircuit.com`, as far as their sources are enabled.

## How it works

- **DigiKey:** Product Information v4 (`/products/v4/search/keyword` and `/productdetails`), signed in with OAuth client credentials. Each product's offer uses the packaging variation that is priced, in stock and sold by DigiKey rather than its marketplace.
- **Mouser:** Search API v1 (`/api/v1/search/keyword` and `/partnumber` with exact matching). Mouser reports most failures, a refused key included, in the body of an HTTP 200; they become errors.
- **jlcsearch:** `/api/search`. It returns only parts in stock, except for an exact LCSC number, whose stock it may not know. It sometimes answers with a transient 500, so the server tries up to three times, half a second and then a second and a half apart.

The tests' DigiKey and Mouser responses are built from the distributors' published API specifications; jlcsearch's are recorded. The nightly live test (`make test-live`) searches jlcsearch, which needs no key. Set `DIGIKEY_CLIENT_ID_FILE` and `DIGIKEY_CLIENT_SECRET_FILE`, or `MOUSER_API_KEY_FILE`, to also test those sources live.
