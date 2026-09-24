# Repository Guidelines

## Project Structure & Module Organization

This Go module (`github.com/kiberdruzhinnik/music-box`) puts the executable in `cmd/music-box/main.go`,
supervisor logic in `pkg/`, and external tests in `test/`. Within `pkg/`,
`config.go`, `subscription.go`, and `links.go` load settings and nodes;
`probe.go` tests nodes; `supervisor.go` handles failover; `process.go` runs
sing-box in-process; and `forwarder.go` keeps HTTP/SOCKS listeners stable.
The pinned `github.com/sagernet/sing-box` module is linked into the binary.
`Dockerfile`, `docker-compose.yml`, and `.env.example` cover deployment;
`helpers/security-scan.sh` runs security scans.

## Build, Test, and Development Commands

Run these commands from the repository root:

```sh
gofmt -w cmd/music-box/*.go pkg/*.go test/*.go
go test ./...                 # run unit tests
go vet ./...                  # inspect suspicious Go constructs
docker build -t local/music-box:latest .
docker compose up -d --build  # run locally using .env
docker compose logs -f
sh helpers/security-scan.sh local/music-box:latest
```

The Docker build supports only `linux/amd64` and `linux/arm64`, runs tagged
integration tests on the native architecture, and packages pinned Cronet in
a non-root Debian 13 distroless image. Copy `.env.example` to `.env` and set
exactly one of `SUBSCRIPTION_URL` and `UPSTREAM_URL`.

## Coding Style & Naming Conventions

Use `gofmt` (tabs) and idiomatic mixed-case Go identifiers. Add a Go-style
comment to every function, including unexported helpers. Keep URLs,
credentials, and subscription contents out of logs and tests. Use
`UPPER_SNAKE_CASE` for environment variables and document defaults in
`.env.example`.

## Testing Guidelines

Name tests `Test...` in `test/*_test.go`. Cover parsing,
configuration, in-process sing-box lifecycle, and failure/fallback paths.
Run `go test ./...` and `go vet ./...` before submitting changes. Rebuild and
test the image after runtime changes; verify a real SOCKS5 request through
Compose and `.env` when changing proxy behavior. Docker healthchecks inspect
the supervisor only; upstream health belongs to its own probes.

## Commit & Pull Request Guidelines

Use short, imperative, lowercase commit subjects such as `fix ...` or
`rename ...`. Keep commits focused. Pull requests should explain the behavior
change, list validation commands and results, call out configuration or image
changes, and include relevant sanitized logs. Do not include secrets or raw
subscription URLs.

## Security & Configuration

Run Semgrep and Trivy through `helpers/security-scan.sh`; review findings rather than
silencing them. Never commit `.env` or credentials. Compose binds host ports
to loopback by default; exposing the unauthenticated proxy to a LAN requires
an explicit binding and firewall rules. Preserve non-root, read-only runtime
compatibility.
