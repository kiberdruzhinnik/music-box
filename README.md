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

## TrueNAS and read-only containers

Temporary sing-box configuration files are stored in `/dev/shm` by default.
If `/tmp` or `/dev/shm` is unavailable, mount a writable ephemeral directory
and set its path with `SB2P_TEMP_DIR`, for example:

```dotenv
SB2P_TEMP_DIR=/run/singbox2proxy-docker
```

The container runs as a non-root user, so the mounted directory must be
writable by that user.

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

Security checks are available through `security-scan.sh` and require Semgrep
and Trivy:

```sh
sh security-scan.sh local/singbox2proxy-docker:latest
```
