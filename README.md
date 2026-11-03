# tailnet-mcp

A stdio MCP server that forwards everything to a Streamable HTTP MCP server inside your tailnet.

Hosted agents (Grok, cloud coding agents, sandboxed assistants) usually take MCP servers two ways. **By URL**: the vendor's servers connect, so your server has to be on the public internet. **By command**: the agent starts a process on its own computer, but that computer is not in your tailnet and you can't run `tailscaled` there (no root, no TUN). tailnet-mcp is the command: it embeds a Tailscale node ([tsnet](https://tailscale.com/kb/1244/tsnet)), joins your tailnet as a tagged device and bridges stdio to the server. Your server stays behind `tailscale serve`, never on the internet.

```
agent ──stdio──▶ tailnet-mcp (tag:agent) ══tailnet══▶ tailscale serve ──▶ MCP server on 127.0.0.1
```

## Install

Download a static binary for linux or darwin, amd64 or arm64, from [Releases](https://github.com/zedong-peng/tailnet-mcp/releases) and check it against `SHA256SUMS`. Or build it yourself:

```bash
CGO_ENABLED=0 go install github.com/zedong-peng/tailnet-mcp@latest
```

Or run the image `ghcr.io/zedong-peng/tailnet-mcp` with a volume on `/home/nonroot/.local/state/tailnet-mcp`. It is also listed in the [MCP Registry](https://registry.modelcontextprotocol.io) as `io.github.zedong-peng/tailnet-mcp`.

## Use

1. Publish the server to the tailnet only, on a path nobody can guess:

   ```bash
   tailscale serve --bg --https=8443 --set-path=/$(openssl rand -hex 16) http://127.0.0.1:8080/mcp
   ```

2. Mint a one-time, pre-authorized auth key with a tag for the agent (admin console, or the API), and log in once from the agent's computer:

   ```bash
   TS_AUTHKEY=tskey-auth-... tailnet-mcp -hostname my-agent -url https://server.tailnet.ts.net:8443/<path> -check
   ```

   `-check` connects, prints the server's tools on stderr and exits. The node key is kept in `-state` (default `~/.local/state/tailnet-mcp/<hostname>`); keep it on persistent storage. Tagged nodes don't expire, so the key is not needed again.

3. Register the same command, without the key, as the agent's MCP server:

   ```bash
   tailnet-mcp -hostname my-agent -url https://server.tailnet.ts.net:8443/<path>
   ```

Each process is its own tailnet node. For a second server, run a second process with a different `-hostname`.

## Lock it down

The agent reads untrusted input (web pages, mail), so assume it can be talked into anything its tools allow. Three layers, none of which rely on the agent behaving:

- **Network.** Give the tag one grant to the ports it needs and nothing else:

  ```jsonc
  "grants": [
    {"src": ["tag:my-agent"], "dst": ["server"], "ip": ["tcp:8443"]},
  ],
  "tests": [
    {"src": "tag:my-agent", "accept": ["server:8443"], "deny": ["server:22", "server:443"]},
  ],
  ```

  Grants that allow everything (`"src": ["autogroup:member"], "dst": ["*"]`) don't cover tagged devices, so that one grant is all the tag gets.

- **Path.** `tailscale serve --set-path` answers only on the secret path; any other path on the port is a 404. Other devices in the tailnet can't use the server by guessing the port.

- **Server.** Enforce permissions where the data lives, not in the agent: a mail server that can only save drafts, a password vault account that is a view-only member of one shared collection. [examples/private-tools](examples/private-tools) sets up both.

## Behaviour

- Before the first request it pings the server's node until it answers. Right after a restart the server may still hold a stale path to the node, and the first dial would time out.
- Only requests that never reached the server are retried, so a tool call is never sent twice.
- When the server restarts and forgets the session (404, or a 400 about the session), the bridge replays `initialize` and resends the request. The client doesn't notice.
- A failed request comes back as a JSON-RPC error with the reason, so the client never waits for an answer that won't come.
- On EOF it ends the session with `DELETE`. Logs go to stderr; stdout carries only MCP frames. `-v` adds tsnet's own logs.

Not supported: the optional GET stream that lets a server send messages outside a response. Servers answer each request in its own response, as almost all tool servers do.

## Compared with

- **`mcp-remote`** and other stdio-to-HTTP bridges need a network path to the server. In an agent's sandbox that means a public URL.
- **`tailscale up` in the sandbox** needs root and a TUN device. If someone logs in as a user instead of with a tagged key, the sandbox gets that user's access to the whole tailnet.
- **[Tailscale Aperture](https://tailscale.com/docs/aperture/mcp-server)** is a gateway on the server side: it aggregates HTTP MCP servers and grants tools per user. It doesn't get a sandboxed agent into the tailnet, so the two fit together.

## Build and test

```bash
go test -race ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" .
```

Tagging `v*` publishes the four binaries, the image and the registry entry.
