# printables

Search [printables.com](https://www.printables.com) for 3D-printable models and get direct download links for their files.

## Tools

| Tool | What it does |
|---|---|
| `search_models` | Search by text, sorted by best match, popularity, date, rating or number of makes. Returns one page of results. |
| `get_model` | One model's description (as Markdown), license, tags, category, pictures, and its files and zip packs. G-code files include their printer, material and print settings. |
| `get_download_links` | Direct download URLs for chosen files or zip packs. They work for 24 hours without signing in. |

The download tool returns links, not file contents. The agent downloads them itself (`curl -fL -o <name> <url>`), so model files never pass through the conversation. The file host rejects the default User-Agent of some HTTP libraries (Go, python-requests). `curl` and `wget` work.

Models sold in the Printables Store can be found and read, but not downloaded.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `MCP_LISTEN_ADDR` | `:8080` | Where to serve MCP (`/mcp`) and the health check (`/healthz`). |
| `PRINTABLES_API_URL` | `https://api.printables.com/graphql/` | The Printables API. Tests point it at a fake. |

The server needs outbound HTTPS to `api.printables.com` only. Agents need `files.printables.com` (downloads) and `media.printables.com` (pictures).

## How it works

It calls the GraphQL API the Printables website uses. That API is undocumented and may change. The nightly live test (`make test-live`) checks the queries against the real service. Printables doesn't require signing in for any of these calls.
