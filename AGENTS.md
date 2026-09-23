# Agent Guide

## Project purpose

This repository builds a Docker image that supervises `singbox2proxy` (`sb2p`) and
`sing-box`. It can run one configured proxy URL or download a subscription,
filter and benchmark its nodes, select the fastest working node, and fail over
when the active proxy becomes unhealthy.

## Repository layout

- `supervisor.py` is the application. It parses subscriptions, probes candidates,
  owns the active proxy subprocess, and handles health checks/failover.
- `docker-entrypoint.sh` starts stable public `socat` listeners, then execs the
  supervisor. Keep it POSIX `sh` compatible.
- `Dockerfile` installs the hash-verified, pinned PyPI `singbox2proxy` wheel and
  downloads the pinned `sing-box` binary during the image build. The runtime image
  must not download either at startup.
- `docker-compose.yml` is the local deployment entry point. `.env` is deliberately
  ignored; `env.example` documents every user-facing setting.
- `.github/workflows/docker-container.yml` tags every push to `main` and publishes
  the image to GHCR.

## Secret handling

- `.env` is a secret-only local file. Never read it, print it, copy it, attach it,
  or send any of its contents to an LLM, issue, log, or other external service.
- Do not use commands that could expose environment values (for example `env`,
  `printenv`, `set`, or `docker compose config` with the real `.env`).
- Use `env.example` and deliberately fake values in tests and documentation. Keep
  `.env` ignored and never stage it.

## Important runtime invariants

- Exactly one of `SUBSCRIPTION_URL` and `UPSTREAM_URL` must be set.
- The supervisor's active listeners stay on loopback using
  `SB2P_INTERNAL_SOCKS_PORT` and `SB2P_INTERNAL_HTTP_PORT`. `socat` alone exposes
  container ports `1080` and `8080`; preserve this separation.
- A probe is successful only when an HTTP GET through the candidate's HTTP proxy
  returns status `204` from `TCP_TEST_URL` (or legacy `TEST_URL`).
- Subscription parsing intentionally supports share-URL lists only: plain text,
  Base64-encoded text, or JSON containing share URLs. Do not silently add partial
  Clash/Mihomo YAML support.
- Do not log proxy or subscription URLs: they can contain credentials. Use the
  existing redaction path for process diagnostics.
- Temporary probe processes require distinct local HTTP and SOCKS ports. Retain
  the reservation/release behavior when changing concurrency or probing.
- `ss://` links with SIP002 plugin options bypass `sb2p` and receive a temporary
  sing-box configuration. Ensure generated files are removed on every exit path.

## Configuration and documentation

- Add or change environment variables in all relevant places: the parsing/default
  in `supervisor.py`, `env.example`, and `README.md`.
- Keep Docker build arguments consistent between `Dockerfile` and
  `docker-compose.yml`. Use exact version pins (and an immutable image digest or
  artifact hash where available); do not use a branch, `main`, `latest`, or a
  floating version range. Remove stale arguments rather than documenting behavior
  the image does not implement.
- When updating `sing-box`, update the SHA-256 entry for every architecture the
  Dockerfile supports (`amd64`, `arm64`, `arm/v7`, `arm/v6`, and `386`).
- Keep `README.md` accurate about supported protocols and subscription formats;
  never place real subscription URLs, credentials, or provider tokens in tracked
  files.

## Development and validation

Run commands from the repository root.

```sh
# Syntax-check the Python application without creating bytecode artifacts.
python3 -c 'from pathlib import Path; compile(Path("supervisor.py").read_text(), "supervisor.py", "exec")'

# Run the isolated supervisor tests; they use mocks and no real proxy credentials.
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -v

# Build and run locally after copying env.example to an untracked .env.
cp env.example .env  # Do not inspect or share this file after adding secrets.
docker compose build
docker compose up -d
docker compose logs -f sb2p
```

Do not commit `.env`. Image builds fetch `sb2p` from GitHub and the selected
`sing-box` release, so use a build as an integration check when dependency or
Dockerfile changes warrant it. For supervisor changes, exercise both direct mode
and subscription mode when credentials/test nodes are available, including an
active-node health-check or failover path when that logic changes.

## Change style

- Use the standard library and retain Python type annotations and dataclasses.
- Add a concise docstring to every Python function and method, including test helpers.
- Prefer narrow, explicit error messages. A failed subscription refresh or probe
  should keep a working active node alive whenever possible.
- Make subprocess lifecycle changes carefully: terminate children, remove any
  temporary config, and preserve SIGTERM/SIGINT behavior.
- This project has no unit-test framework currently. Add focused tests only with
  a justified test dependency; otherwise run the syntax and Compose checks above.
