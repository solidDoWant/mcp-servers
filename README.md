# mcp-servers

Small [MCP](https://modelcontextprotocol.io) servers, written in Go and run behind the [toolgate](https://github.com/solidDoWant/toolgate) gateway. Each server is a folder under `servers/`, with its own version and its own image.

| Server | What it does | Image |
|---|---|---|
| [electronic-components](servers/electronic-components/README.md) | Search DigiKey, Mouser and JLCPCB's parts library for electronic components and compare offers | `ghcr.io/soliddowant/mcp-electronic-components` |
| [printables](servers/printables/README.md) | Search printables.com and get direct download links for model files | `ghcr.io/soliddowant/mcp-printables` |
| [sendcutsend](servers/sendcutsend/README.md) | Upload flat parts to SendCutSend, price them, and build saved carts to check out in a browser | `ghcr.io/soliddowant/mcp-sendcutsend` |

## Running a server

Each image serves MCP over Streamable HTTP at `http://<host>:8080/mcp`, with a health check at `/healthz`:

```sh
docker run --rm -p 8080:8080 ghcr.io/soliddowant/mcp-printables:<version>
claude mcp add --transport http printables http://localhost:8080/mcp
```

The servers keep no state, so you can run as many replicas as you like. Each server's README lists its settings.

## Development

See [CLAUDE.md](CLAUDE.md) for the commands and [docs/server-contract.md](docs/server-contract.md) for the rules every server follows.

## License

[AGPL-3.0](LICENSE)
