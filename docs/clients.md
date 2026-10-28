# Connecting clients

Every managed server gets its own endpoints. There is no aggregated `/mcp`
endpoint; point each client at the server it needs.

| URL | Protocol |
|---|---|
| `http://HOST:8080/servers/NAME/mcp` | Streamable HTTP (current and legacy revisions). POST only; `GET` and `DELETE` answer 405. |
| `http://HOST:8080/servers/NAME/sse` | Legacy HTTP+SSE (revision 2024-11-05), for older clients. |

Use `https://` when TLS is enabled. The server page in the UI shows these
commands with your `public_url` (or the address you browsed to) filled in.

An endpoint answers only while the server is `ready`. While it is starting,
failed, or stopped, the route returns 404 `{"error":"unknown server"}`. Check
the dashboard or `GET /api/servers`.

## Claude Code

Streamable HTTP, the preferred option:

```sh
claude mcp add --transport http filesystem http://mcp.lan:8080/servers/filesystem/mcp
claude mcp list
```

Legacy SSE, if you need it (Claude Code marks SSE deprecated):

```sh
claude mcp add --transport sse filesystem http://mcp.lan:8080/servers/filesystem/sse
```

## stdio clients: `mcp-machine connect`

For clients that can only launch a command, the same binary bridges stdio to a
Streamable HTTP endpoint:

```sh
mcp-machine connect http://mcp.lan:8080/servers/filesystem/mcp
```

Standard output carries only MCP messages; logs go to standard error. With
Claude Code:

```sh
claude mcp add filesystem -- mcp-machine connect http://mcp.lan:8080/servers/filesystem/mcp
```

In a JSON client config (Claude Desktop, Cursor, and similar):

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "mcp-machine",
      "args": ["connect", "http://mcp.lan:8080/servers/filesystem/mcp"]
    }
  }
}
```

`connect` is the same binary that runs in the container. To get it on a laptop,
build it from the repository with `make build` (the binary is `bin/mcp-machine`).
Cross-platform packaging of the binary is not provided. For an `https://`
endpoint with a self-signed certificate, the machine running `connect` must
trust that certificate; MCP Machine does not handle trust for you.

## Alternative bridge: mcp-remote

`mcp-remote` also bridges stdio to a remote endpoint and supports the current
protocol revision:

```sh
npx mcp-remote http://mcp.lan:8080/servers/filesystem/mcp
```

Some versions of `mcp-remote` refuse plain `http://` URLs for hosts other than
localhost unless you add `--allow-http`; use TLS or that flag on a trusted LAN.

## Browser-based clients and origins

The MCP routes validate the `Origin` header:

- requests with no `Origin` header (CLIs, SDKs, `connect`) pass;
- same-origin requests pass;
- a cross-origin request passes only if its origin is listed in
  `allowed_origins` or equals the origin of `public_url`. Otherwise MCP Machine
  answers 403 `{"jsonrpc":"2.0","error":{"code":-32000,"message":"origin not allowed"}}`.

So a web app served from `http://app.lan:3000` that talks to MCP Machine needs:

```yaml
public_url: "http://mcp.lan:8080"
allowed_origins:
  - "http://app.lan:3000"
```

Origins are exact matches on scheme, host, and port. Set `public_url` to the
address clients actually use: besides being trusted, it appears in the connect
snippets. There is no authentication in v1, so expose the port only on networks
you trust.

The Go SDK's DNS-rebinding protection also stays on: a request that arrives on
a loopback address with a non-loopback `Host` header is rejected.

## Checking from the command line

```sh
curl -s http://mcp.lan:8080/api/servers          # JSON list with each server's state
curl -N http://mcp.lan:8080/servers/filesystem/sse   # prints an "event: endpoint" line
```
