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

## Tests

- **Unit tests** call the tools through a real MCP client (`internal/mcptest`) against a fake upstream (`httptest`) with recorded responses in `testdata/`. They cover the mapping of each field, input validation, and every kind of upstream failure.
- **A tool-list golden file** (`testdata/tools.golden.json`) holds every tool's name, description and schemas. Any change to what clients see shows up as a diff in review. Rewrite it with `make golden`.
- **Live tests** (build tag `live`, test names containing `Live`) call the real upstream service. CI runs them every night, because most of these servers wrap undocumented APIs that can change without notice.

## Releases

- Each server has its own version and its own `CHANGELOG.md`.
- Push the tag `<name>/v<semver>` (for example `printables/v0.1.0`) to publish the image. The release workflow tests the code, pushes the image and creates a GitHub release.
- The homelab deploys servers from the toolgate catalog in `solidDoWant/infra-mk3` (`cluster/gitops/ai/toolgate/catalog/`), with the image pinned by digest. Renovate there opens a PR when a new version is published.
