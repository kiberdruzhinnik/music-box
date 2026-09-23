# singbox2proxy-docker

`singbox2proxy-docker` runs a local HTTP and SOCKS5 proxy backed by `sing-box`.
It can use one configured upstream URL or select the fastest working node from
a subscription.

## Quick start

1. Copy `.env.example` to `.env`.
2. Set exactly one of `SUBSCRIPTION_URL` or `UPSTREAM_URL`.
3. Set `COUNTRY` or `COUNTRY_REGEX` when using a subscription.
4. Start the container:

```sh
cp .env.example .env
docker compose up -d --build
docker compose logs -f
```

The host ports are configured by `SOCKS5_PORT` and `HTTP_PORT` (10800 and
10801 in the example). Inside the container, the HTTP and SOCKS5 services are
available on ports 8080 and 1080.

By default, Compose publishes the host ports on `127.0.0.1`, so only programs
running on the Docker host can connect. To use the proxy from another device,
set `BIND_ADDRESS` in `.env` to the host's LAN IP (or `0.0.0.0`) and restart
with `docker compose up -d`. The proxy has no client authentication: restrict
these ports to trusted clients with a firewall, and never expose them to the
public Internet. In TrueNAS Apps, configure the app's port publishing in the
TrueNAS UI; `BIND_ADDRESS` only controls this repository's Compose file.

After a log line beginning `ACTIVE`, verify the host-side SOCKS5 port with:

```sh
curl --proxy socks5h://127.0.0.1:10800 \
  https://www.gstatic.com/generate_204 -o /dev/null -w '%{http_code}\n'
```

A working proxy prints `204`. Before an upstream is activated, the container
may be running and its port may accept TCP connections, but proxy requests
will fail; check `docker compose logs` for `ACTIVE` and the `alive=X/Y` retest
summary. Use the published host port, not the container's internal port 1080.

## How it works

In subscription mode, the supervisor:

- downloads the subscription through the active upstream proxy when possible;
- falls back to a direct download when no upstream is healthy;
- accepts plain URI lists, Base64-encoded URI lists, and JSON containing URIs;
- filters nodes by country or regular expression;
- tests candidates concurrently with an HTTP GET to `TCP_TEST_URL`;
- accepts only an HTTP 204 response and selects the lowest successful RTT;
- logs a summary such as `filtered=20, alive=3/20` after each retest.

The active proxy is checked independently at `HEALTHCHECK_INTERVAL_SECONDS`.
Failures are retried according to `HEALTHCHECK_FAILURE_THRESHOLD` and
`HEALTHCHECK_RETRY_DELAY_SECONDS` before failover. If no candidate works, the
subscription and benchmark are retried after `UNAVAILABLE_RETRY_SECONDS`.

In direct mode, the configured `UPSTREAM_URL` is supervised and restarted when
it stops working.

The Docker healthcheck verifies that the supervisor is running. It does not
fetch `TCP_TEST_URL`; upstream testing and failover belong to the supervisor.

## Configuration

The complete list of settings and defaults is in `.env.example`. The most
important settings are:

```dotenv
SUBSCRIPTION_URL=https://provider.example/subscription/token
UPSTREAM_URL=
COUNTRY=Netherlands
COUNTRY_REGEX=
REVERSE_MATCHES=true
TCP_TEST_URL=https://www.gstatic.com/generate_204
PROBE_TIMEOUT_SECONDS=8
PROBE_ATTEMPTS=2
PROBE_CONCURRENCY=4
HEALTHCHECK_INTERVAL_SECONDS=60
HEALTHCHECK_FAILURE_THRESHOLD=2
HEALTHCHECK_RETRY_DELAY_SECONDS=2
```

`COUNTRY_REGEX` takes precedence over `COUNTRY`. For example:

```dotenv
COUNTRY=
COUNTRY_REGEX=(?i)(🇳🇱|Netherlands|\bNL\b)
```

`TEST_URL` is accepted as a backwards-compatible alias for
`TCP_TEST_URL`. The probe endpoint must return HTTP 204 through a working
proxy.

## Supported URLs

VLESS, VMess, Trojan, Hysteria 1/2, Shadowsocks (including SIP002 plugin
options), TUIC, WireGuard, SSH, HTTP/HTTPS, SOCKS4/5, and Naive HTTPS share
URLs are supported. Clash/Mihomo YAML proxy objects are not converted; use a
subscription endpoint that returns share URLs.

## Build and verify

The image supports `linux/amd64` and `linux/arm64` only.

```sh
go test ./...
go vet ./...
docker build -t local/singbox2proxy-docker:latest .
```

sing-box is a pinned Go module dependency (`github.com/sagernet/sing-box`
v1.14.1). The Docker build links it into the supervisor, so no separate
sing-box executable or temporary config directory is required. Naive support
uses the separately pinned Cronet shared library in the container image.

Security checks are available through `security-scan.sh` and require Semgrep
and Trivy:

```sh
sh security-scan.sh local/singbox2proxy-docker:latest
```
