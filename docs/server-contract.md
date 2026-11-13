# The server contract

Every server in this repository follows these rules, so that each one deploys, runs and is tested the same way. Reviewers check a new server against this page.

## Shape

- One Go package `main` in `servers/<name>/`, with its logic in `servers/<name>/internal/<name>/`. Code that two servers need goes in `internal/`.
- `main` calls `mcpserver.Main` and does nothing else. Configuration comes from environment variables, and secrets come from files whose paths are set in environment variables. Never put a secret in a command-line argument.
- The image is `ghcr.io/soliddowant/mcp-<name>`. It contains the static binary on distroless, running as a non-root user, so it works with a read-only root filesystem and all capabilities dropped. Builds use `ko`, so a server has no Dockerfile.

## Protocol

- MCP over Streamable HTTP at `/mcp`, on port 8080 (`MCP_LISTEN_ADDR` overrides it), plus `GET /healthz` for probes.
- **Stateless by default.** The server keeps nothing between requests, so any replica can answer any request and a restart loses nothing. A server that really needs per-session state says why in its README.
- Logs are JSON on stdout.

## Tools

- **Failures are tool errors.** When an upstream call fails, return an error with a message that tells the model what went wrong (for example `Printables returned HTTP 403 …`). An empty result must mean that nothing was found, never that the request failed.
- **Bounded output.** Every list takes a limit with a sensible default and maximum. Long text, such as descriptions, is cut at a stated length. Return compact structured data, never raw HTML.
- **No bulk data in the conversation.** When a tool's result is a file, return a URL the agent can download (`curl -fL -o …`), not the bytes. Use the source site's own links when it has them.
- **Validate before calling upstream.** Reject bad input (unknown IDs, out-of-range limits) with a clear error and without making the upstream request.
- **Descriptions are the interface.** Tool and parameter descriptions are all the model knows about the server. Say what each tool is for, what the IDs it returns are for, and the limits that apply. Mark read-only tools with `ReadOnlyHint`.
- **Honest upstream identity.** Send a User-Agent naming the server and this repository unless the upstream refuses it.

## Caching

Cache when an upstream has a small quota or is slow, and its answers stay true for a while. Use `internal/cache`, never a home-made cache:

- **Backends.** Memory by default. A shared Redis-protocol server when `CACHE_URL_FILE` names a file holding its `redis://` or `rediss://` URL; answers and daily budgets then survive restarts and are shared by replicas. The package uses standard commands only (GET, SET with PX, INCRBY, EXPIREAT, MULTI/EXEC), so any Redis-compatible server works. Don't rely on one server's extensions.
- **What to cache.** Successful answers only. Give "not found" a short lifetime. Put a schema version in every key, and bump it when the shape of a cached value changes. Keys are hashed, so the store never holds the queries themselves.
- **Stale answers.** Keep entries past their lifetime (`CACHE_STALE_FOR`), and when the upstream fails, answer with the old entry and say so: its age, that it's stale, and the failure.
- **Budgets.** Count each upstream's calls per day with the package's budgets, below the upstream's own limits. With a shared store the count is exact across restarts and replicas; without one it resets on restart.
- **Say where answers came from.** Tool output says which answers came from the cache, and how old they are.

### A shared server

Several servers may share one Redis-protocol server. Give each its own ACL user, limited to its key prefix (`CACHE_KEY_PREFIX`, the server's name by default) and channel prefix, with commands that list or count other servers' keys denied:

```
ACL DELUSER electronic-components
ACL SETUSER electronic-components on >PASSWORD ~electronic-components:* &electronic-components:* +@all -@dangerous -@admin -scan -randomkey -dbsize
```

Delete and recreate a user rather than use the `reset` rule, which some servers (Dragonfly) don't support. Without the extra denies, `SCAN` and `RANDOMKEY` return other servers' key names and `DBSIZE` counts their keys. CI checks this isolation against Valkey and Dragonfly (`internal/cache`, `TestServer…`).

The ACL isolates data, not resources: one server can fill the store's memory and push out another's entries. That's fine for caches, which can always be fetched again.


- **Unit tests** call the tools through a real MCP client (`internal/mcptest`) against a fake upstream (`httptest`) with recorded responses in `testdata/`. They cover the mapping of each field, input validation, and every kind of upstream failure.
- **A tool-list golden file** (`testdata/tools.golden.json`) holds every tool's name, description and schemas. Any change to what clients see shows up as a diff in review. Rewrite it with `make golden`.
- **Live tests** (build tag `live`, test names containing `Live`) call the real upstream service. CI runs them every night, because most of these servers wrap undocumented APIs that can change without notice.

## Releases

- Each server has its own version and its own `CHANGELOG.md`.
- Push the tag `<name>/v<semver>` (for example `printables/v0.1.0`) to publish the image. The release workflow tests the code, pushes the image and creates a GitHub release.
- The homelab deploys servers from the toolgate catalog in `solidDoWant/infra-mk3` (`cluster/gitops/ai/toolgate/catalog/`), with the image pinned by digest. Renovate there opens a PR when a new version is published.
