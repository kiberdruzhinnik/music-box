# Repository Guidelines

## Project Structure & Module Organization

This is a Go module (`singbox2proxy-docker`) with all production sources in
the repository root. `main.go` starts the supervisor, while `config.go`,
`subscription.go`, `links.go`, `probe.go`, `process.go`, `supervisor.go`, and
`forwarder.go` contain configuration, URI parsing, probing, sing-box process
management, failover, and proxy forwarding. Go tests are kept beside the code
(`*_test.go`). `Dockerfile`, `docker-compose.yml`, `.env.example`, and
`security-scan.sh` define the container and local operations. Never commit
`.env` or other credentials.

## Build, Test, and Development Commands

Run these commands from the repository root:

```sh
go test ./...                  # run all unit and integration-style tests
go vet ./...                   # inspect suspicious Go constructs
docker build -t local/singbox2proxy-docker:latest .
docker compose up -d --build   # run locally using .env
docker compose logs -f
sh security-scan.sh local/singbox2proxy-docker:latest
```

The Docker build embeds the pinned sing-box release and supports only
`linux/amd64` and `linux/arm64`. Use `.env.example` as the configuration
template; set exactly one of `SUBSCRIPTION_URL` and `UPSTREAM_URL`.

## Coding Style & Naming Conventions

Use standard Go formatting (`gofmt`) with tabs and idiomatic mixed-case Go
identifiers. Exported declarations require Go-style comments; document helper
functions when their behavior is non-obvious. Keep URLs, credentials, and
subscription contents out of logs and tests. Add configuration names in
`UPPER_SNAKE_CASE` and keep defaults documented in `.env.example`.

## Testing Guidelines

Name tests `Test...` and place them in `*_test.go` beside the implementation.
Cover parsing, configuration, process lifecycle, and failure/fallback paths.
Run `go test ./...` and `go vet ./...` before submitting changes; rebuild the
image when Dockerfile or runtime behavior changes.

## Commit & Pull Request Guidelines

Use short, imperative, lowercase commit subjects such as `fix ...` or
`rename ...`. Keep commits focused. Pull requests should explain the behavior
change, list validation commands and results, call out configuration or image
changes, and include relevant sanitized logs. Do not include secrets or raw
subscription URLs.

## Security & Configuration

Run Semgrep and Trivy through `security-scan.sh`. Review findings rather than
silencing them. The non-root container may need a writable `SB2P_TEMP_DIR` on
TrueNAS or other read-only runtimes.
