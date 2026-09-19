#!/bin/sh
set -eu

SB2P_INTERNAL_SOCKS_PORT="${SB2P_INTERNAL_SOCKS_PORT:-11080}"
SB2P_INTERNAL_HTTP_PORT="${SB2P_INTERNAL_HTTP_PORT:-18080}"

case "$SB2P_INTERNAL_SOCKS_PORT" in *[!0-9]*|'') echo "Invalid SB2P_INTERNAL_SOCKS_PORT" >&2; exit 2;; esac
case "$SB2P_INTERNAL_HTTP_PORT" in *[!0-9]*|'') echo "Invalid SB2P_INTERNAL_HTTP_PORT" >&2; exit 2;; esac

# Stable container-facing listeners. The active sb2p process remains loopback-only.
socat TCP-LISTEN:1080,bind=0.0.0.0,reuseaddr,fork TCP:127.0.0.1:"$SB2P_INTERNAL_SOCKS_PORT" &
socat TCP-LISTEN:8080,bind=0.0.0.0,reuseaddr,fork TCP:127.0.0.1:"$SB2P_INTERNAL_HTTP_PORT" &

exec python /usr/local/bin/supervisor.py "$@"
