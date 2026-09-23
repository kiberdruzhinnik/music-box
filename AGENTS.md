# Repository Guidelines

## Project Structure & Module Organization

This is one Go module (`singbox2proxy-docker`), with source and `*_test.go`
files in the repository root. `main.go` starts the supervisor; `config.go`,
`subscription.go`, and `links.go` load settings and nodes. `probe.go` tests
nodes, `supervisor.go` handles selection and failover, `process.go` runs
sing-box in-process, and `forwarder.go` keeps public HTTP/SOCKS listeners
stable across node changes. The pinned `github.com/sagernet/sing-box` module
is linked into the executable; there is no separate sing-box binary or
temporary JSON config. `Dockerfile`, `docker-compose.yml`, `.env.example`,
and `security-scan.sh` cover deployment and scanning.

## Build, Test, and Development Commands

Run these commands from the repository root:

```sh
gofmt -w *.go                 # format changed Go files
go test ./...                 # run unit tests
go vet ./...                  # inspect suspicious Go constructs
docker build -t local/singbox2proxy-docker:latest .
docker compose up -d --build  # run locally using .env
docker compose logs -f
sh security-scan.sh local/singbox2proxy-docker:latest
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

Name tests `Test...` in `*_test.go` beside the implementation. Cover parsing,
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

Run Semgrep and Trivy through `security-scan.sh`; review findings rather than
silencing them. Never commit `.env` or credentials. Compose binds host ports
to loopback by default; exposing the unauthenticated proxy to a LAN requires
an explicit binding and firewall rules. Preserve non-root, read-only runtime
compatibility.
