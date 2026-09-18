# singbox2proxy Docker wrapper

A small Docker wrapper around `nichind/singbox2proxy` (`sb2p`). It accepts a
share URL in `UPSTREAM_URL` and exposes stable SOCKS5 and HTTP ports.

`sing-box` is downloaded **only while building the image** and copied into the
runtime image as `/usr/local/bin/sing-box`. Container starts/restarts do not
download sing-box.

## Start

```sh
cp .env.example .env
# edit UPSTREAM_URL and ports
docker compose build
docker compose up -d
```

With the example `.env`:

- SOCKS5: `socks5://127.0.0.1:10800`
- HTTP: `http://127.0.0.1:10801`

Test:

```sh
curl --proxy socks5h://127.0.0.1:10800 https://api.ipify.org
curl --proxy http://127.0.0.1:10801 https://api.ipify.org
```

## Example upstream URLs

```dotenv
UPSTREAM_URL='vless://...'
UPSTREAM_URL='vmess://...'
UPSTREAM_URL='trojan://...'
UPSTREAM_URL='hysteria2://...'
UPSTREAM_URL='hy2://...'
```

Use only one `UPSTREAM_URL` at a time.

## Expose to a LAN / Keenetic

Change:

```dotenv
BIND_ADDRESS=0.0.0.0
SOCKS5_PORT=10800
HTTP_PORT=10801
```

Then clients can connect to `<docker-host-ip>:10800` (SOCKS5) or
`<docker-host-ip>:10801` (HTTP).

The proxy listeners themselves have no authentication in this wrapper. Do not
publish them directly to the Internet; use host firewall rules if binding to
`0.0.0.0`.

## Updating

Rebuild explicitly when you want to update `sb2p` or sing-box:

```sh
docker compose build --no-cache
docker compose up -d
```

For reproducible builds, set `SB2P_REF` to a specific Git commit or tag instead
of `main`.

## Why socat is present

`sb2p` binds its generated HTTP/SOCKS listeners to loopback. Docker published
ports reach the container network interface, not `127.0.0.1`, so the entrypoint
uses two small TCP forwarders:

```text
host SOCKS5_PORT -> container :1080 -> 127.0.0.1:11080 -> sb2p -> upstream
host HTTP_PORT   -> container :8080 -> 127.0.0.1:18080 -> sb2p -> upstream
```
