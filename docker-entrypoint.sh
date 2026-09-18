#!/bin/sh
set -eu

: "${UPSTREAM_URL:?UPSTREAM_URL is required, e.g. vless://..., vmess://..., trojan://..., hysteria2://...}"

SB2P_INTERNAL_SOCKS_PORT="${SB2P_INTERNAL_SOCKS_PORT:-11080}"
SB2P_INTERNAL_HTTP_PORT="${SB2P_INTERNAL_HTTP_PORT:-18080}"

case "$SB2P_INTERNAL_SOCKS_PORT" in *[!0-9]*|'') echo "Invalid SB2P_INTERNAL_SOCKS_PORT" >&2; exit 2;; esac
case "$SB2P_INTERNAL_HTTP_PORT" in *[!0-9]*|'') echo "Invalid SB2P_INTERNAL_HTTP_PORT" >&2; exit 2;; esac

# sb2p currently exposes its generated local proxies on loopback. Docker's
# published ports cannot reach a process bound to 127.0.0.1 inside the
# container, so forward stable container-facing ports to sb2p's private ports.
socat TCP-LISTEN:1080,bind=0.0.0.0,reuseaddr,fork TCP:127.0.0.1:"$SB2P_INTERNAL_SOCKS_PORT" &
socks_forwarder_pid=$!

socat TCP-LISTEN:8080,bind=0.0.0.0,reuseaddr,fork TCP:127.0.0.1:"$SB2P_INTERNAL_HTTP_PORT" &
http_forwarder_pid=$!

cleanup() {
  kill "$socks_forwarder_pid" "$http_forwarder_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# sing-box is already in PATH from the image build. sb2p therefore has no
# reason to fetch sing-box at runtime.
exec sb2p "$UPSTREAM_URL" \
  --socks-port "$SB2P_INTERNAL_SOCKS_PORT" \
  --http-port "$SB2P_INTERNAL_HTTP_PORT" \
  "$@"
