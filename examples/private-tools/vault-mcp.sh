#!/usr/bin/env bash
# vault-mcp@<agent> entry: unlock the agent's Vaultwarden account with bw, then serve the Bitwarden MCP
# over Streamable HTTP on loopback. The master password stays in this shell; the MCP only gets BW_SESSION.
set -euo pipefail
export PATH=/opt/mcp/vault/bin:/opt/node/bin:$PATH
export BW_NOINTERACTION=true

case $(bw status | jq -r .status) in
  unauthenticated)
    # bw only speaks HTTPS, so it goes through tailscale serve instead of straight to 127.0.0.1:8222.
    bw config server "$VAULT_URL" >/dev/null
    BW_SESSION=$(bw login "$VAULT_EMAIL" --passwordenv VAULT_PASSWORD --raw) ;;
  *)
    BW_SESSION=$(bw unlock --passwordenv VAULT_PASSWORD --raw) ;;
esac
export BW_SESSION
unset VAULT_PASSWORD VAULT_EMAIL VAULT_URL
bw sync >/dev/null

exec supergateway --stdio mcp-server-bitwarden --outputTransport streamableHttp --stateful \
  --sessionTimeout 1800000 --host 127.0.0.1 --port "$PORT" --streamableHttpPath /mcp \
  --healthEndpoint /healthz --logLevel info
