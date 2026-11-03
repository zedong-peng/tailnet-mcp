# Mail and passwords for a hosted agent

A worked setup: one small Linux server in your tailnet gives agents your IMAP mail and a password vault. Each permission is enforced by the server holding the data, so an agent that gets prompt-injected by an email still can't send mail or change a password.

```
agent ──stdio──▶ tailnet-mcp (tag:my-agent) ══▶ server :8443/<secret> ──▶ draftmail    (read + draft)
agent ──stdio──▶ tailnet-mcp (tag:my-agent) ══▶ server :8444/<secret> ──▶ vault MCP    (view-only account)
your devices ══════════════════════════════════▶ server :443           ──▶ Vaultwarden
```

It runs on a 1 GB VPS. These are the pieces from a working deployment; adjust paths and ports to taste.

## Mail: read and draft, never send

[draftmail](https://github.com/zedong-peng/draftmail) has three tools: search, read and draft. A draft (new, reply or forward) goes to your Drafts folder; you read it and press send yourself. There is no code to send, move or delete, so there is nothing to misconfigure. It is one static binary and needs only IMAP.

```bash
EMAIL_CREDENTIALS='me@icloud.com:app-password,me@uni.edu:password:imap.uni.edu' \
  draftmail -http 127.0.0.1:18443 -allow-host server.tailnet.ts.net
```

Run it as a systemd service with the credentials in an `EnvironmentFile`.

## Passwords: a view-only account

[Vaultwarden](https://github.com/dani-garcia/vaultwarden) holds the vault; your own devices use the normal Bitwarden apps against `https://server.tailnet.ts.net`. The agent gets **its own account**, invited into an organization collection with *Can view* permission. Vaultwarden rejects every edit or delete from it, whatever the MCP's tool list says.

- Run Vaultwarden on `127.0.0.1:8222` with `SIGNUPS_ALLOWED=false`, `SENDS_ALLOWED=false` and `ORG_CREATION_USERS=<you>`, and publish it with `tailscale serve --bg --https=443 http://127.0.0.1:8222`. It has no release binaries; the `-alpine` image carries a static one you can copy out.
- Signups are closed and `bw` can't register accounts, so [vault-account.py](vault-account.py) invites you and registers the agent's account the way the web vault would.
- In the web vault: create an organization and a collection, invite the agent's address to it as *User* with *Can view*, then confirm it.

The MCP is [Bitwarden's own](https://github.com/bitwarden/mcp-server). It speaks stdio only, so [supergateway](https://github.com/supercorp-ai/supergateway) serves it over Streamable HTTP. [vault-mcp.sh](vault-mcp.sh) unlocks the agent's account, keeps the master password out of the MCP's environment and starts both. [vault-mcp@.service](vault-mcp@.service) is a template: one instance, Vaultwarden account and port per agent.

Two details that cost time:

- `bw` refuses plain `http://`, even on loopback, so it talks to Vaultwarden through `tailscale serve`. A host without MagicDNS needs its own tailnet name in `/etc/hosts`.
- The unit allows only loopback and the host's tailnet address (`IPAddressDeny=any`), so the MCP can't reach anything else.

## Publish and lock down

```bash
tailscale serve --bg --https=8443 --set-path=/$MAIL_SECRET  http://127.0.0.1:18443/mcp
tailscale serve --bg --https=8444 --set-path=/$VAULT_SECRET http://127.0.0.1:18444/mcp
```

```jsonc
"grants": [
  {"src": ["tag:my-agent"], "dst": ["server"], "ip": ["tcp:8443", "tcp:8444"]},
],
"tests": [
  {"src": "tag:my-agent", "accept": ["server:8443", "server:8444"], "deny": ["server:443", "server:22"]},
],
```

On the agent's side, two tailnet-mcp commands, one per server, each with its own `-hostname`:

```bash
tailnet-mcp -hostname my-agent-mail  -url https://server.tailnet.ts.net:8443/$MAIL_SECRET
tailnet-mcp -hostname my-agent-vault -url https://server.tailnet.ts.net:8444/$VAULT_SECRET
```
