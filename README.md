# tailnet-mcp

A stdio MCP server that forwards everything to a Streamable HTTP MCP server inside your tailnet.

It embeds a Tailscale node ([tsnet](https://tailscale.com/kb/1244/tsnet)), so the machine running it needs no root, no TUN device and no `tailscaled`. That makes it usable from hosts that can only launch MCP servers as commands, such as a hosted agent's cloud computer, while the real server stays off the public internet behind `tailscale serve`.

```
agent ──stdio──▶ tailnet-mcp ══tailnet══▶ tailscale serve ──▶ MCP server (127.0.0.1)
```

## Use

```bash
# first run only: join the tailnet with a pre-authorized, tagged auth key
TS_AUTHKEY=tskey-auth-... tailnet-mcp -hostname my-agent -url https://server.tailnet.ts.net:8443/mcp -check

# then register this as the MCP server command (no key needed any more)
tailnet-mcp -hostname my-agent -url https://server.tailnet.ts.net:8443/mcp
```

- The node key is kept in `-state` (default `~/.local/state/tailnet-mcp/<hostname>`). Keep that directory on persistent storage; tagged nodes do not expire.
- `-check` connects, prints the server's tools on stderr and exits.
- Before the first request it pings the server's node until it answers, so a cold start does not hit a stale path.
- Logs go to stderr; stdout carries only MCP frames.

Lock the tag down in your tailnet policy so the node can reach that one port and nothing else:

```jsonc
"grants": [
  {"src": ["tag:my-agent"], "dst": ["server"], "ip": ["tcp:8443"]},
],
```

## Build

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" .
```

Releases ship static binaries for linux and darwin, amd64 and arm64.
