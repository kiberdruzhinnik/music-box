# go-singbox2proxy

A Go implementation of the sibling Python subscription supervisor. It talks directly to a pinned `sing-box` binary: there is no Python, `singbox2proxy` package, or `socat` in this image. The original implementation is left unchanged for comparison or rollback.

## Run

Use the parent directory's `.env` file (the Compose file references it). To run alongside the Python container, override the **host** ports so they do not collide:

```sh
SOCKS5_PORT=20800 HTTP_PORT=20801 docker compose up -d --build
SOCKS5_PORT=20800 HTTP_PORT=20801 docker compose ps
```

The container always publishes SOCKS5 on port `1080` and HTTP proxy on port `8080`; Go forwards those stable sockets to loopback-only sing-box listeners. The Docker healthcheck only checks that the Go supervisor process is alive. It deliberately does not probe `TCP_TEST_URL`—node selection and failover are the supervisor's responsibility.

`SUBSCRIPTION_URL` and `UPSTREAM_URL` remain mutually exclusive. The Go version honors the parent `.env.example` settings, including `COUNTRY`, `COUNTRY_REGEX`, `REVERSE_MATCHES`, refresh and probe intervals, probe concurrency and attempts, active-healthcheck threshold and retry delay, unavailable retry, and internal port overrides. `TCP_TEST_URL` (or legacy `TEST_URL`) must answer with HTTP 204 through a usable node.

In subscription mode it downloads plain, Base64, or JSON share-link lists; filters and optionally reverses the matches; probes candidates in parallel; selects the best measured RTT; and logs `retest summary: filtered=Y, alive=X/Y` after each benchmark. A subscription refresh uses the active HTTP proxy when that proxy passes an immediate probe, or goes direct when none is healthy. Active-node checks require consecutive failures before ranked failover. If no node can be activated, both refresh and benchmark retry on the shorter unavailable cadence. Direct mode supervises one URL and restarts it on failure.

The Docker image builds only for `linux/amd64` and `linux/arm64`. Other target architectures fail at build time.

Supported share URLs: VLESS, VMess, Trojan, Hysteria 1/2, Shadowsocks (including SIP002 plugin options), TUIC, WireGuard, SSH, HTTP/HTTPS, SOCKS 4/5, and Naive HTTPS. The image uses sing-box's static musl release, which includes Naive/Cronet support on both supported architectures. WireGuard URLs generate sing-box 1.14 endpoints, not the removed outbound form. As in the Python version, Clash/Mihomo YAML proxy objects are not accepted; use a subscription endpoint that emits share URLs.

## Verify

```sh
go test ./...
go vet ./...
docker build -t local/go-singbox2proxy:1.0.0 .
```

The Docker build runs the Go tests. On native builds, it also checks generated share-link configurations with the bundled sing-box; cross-builds skip executing the target-architecture binary. The runtime is `FROM scratch` with just the two statically linked executables, CA certificates, and a writable temporary directory. It runs as a non-root UID.

## Security scans

Run `sh security-scan.sh` after building the image. The script requires Semgrep and Trivy on `PATH`, scans this subfolder (never the parent `.env`) with Semgrep's default, security-audit, and OWASP Top Ten rulesets, then runs Trivy vulnerability, misconfiguration, and secret scans on both the source tree and image. Findings cause a nonzero exit. Pass an image name as the first argument to scan a different tag.

The current pinned sing-box 1.14.1 release still embeds dependencies with upstream Trivy advisories. The script reports these rather than suppressing them; upgrading to an untested pre-release or ignoring findings would not be a safe fix.
