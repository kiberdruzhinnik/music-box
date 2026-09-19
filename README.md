# singbox2proxy subscription supervisor

This image keeps `sing-box` baked into the Docker image and adds a Python supervisor in front of `sb2p`.

## Behaviour

1. Periodically downloads `SUBSCRIPTION_URL`.
2. Parses plain share-link lists, Base64 share-link lists, or JSON containing share links.
3. Filters nodes by `COUNTRY` or `COUNTRY_REGEX` using the node display name.
4. Reverses matched subscription order by default.
5. Periodically launches temporary `sb2p` instances for candidate nodes.
6. For each candidate, performs HTTP GET through its HTTP proxy to `https://www.gstatic.com/generate_204`.
7. Only HTTP 204 counts as working; the candidate's score is its lowest measured RTT.
8. Selects the lowest-RTT working node and runs it on the stable internal ports.
9. Existing Docker SOCKS5/HTTP ports remain unchanged via the `socat` forwarding layer.
10. If the active process exits, an immediate benchmark/failover is scheduled.

## Configuration

Copy `.env.example` to `.env`, set the subscription URL and country, then build/run:

```sh
cp .env.example .env
docker compose build
docker compose up -d
docker compose logs -f
```

Example:

```dotenv
SUBSCRIPTION_URL='https://provider.example/subscription/token'
UPSTREAM_URL=
COUNTRY=Netherlands
REVERSE_MATCHES=true
SUBSCRIPTION_REFRESH_SECONDS=3600
PROBE_INTERVAL_SECONDS=300
TCP_TEST_URL=https://www.gstatic.com/generate_204
PROBE_TIMEOUT_SECONDS=8
PROBE_ATTEMPTS=2
PROBE_CONCURRENCY=4
SOCKS5_PORT=10800
HTTP_PORT=10801
BIND_ADDRESS=0.0.0.0
```

For providers whose labels vary, regex filtering is safer:

```dotenv
COUNTRY=
COUNTRY_REGEX='(🇳🇱|Netherlands|\bNL\b)'
```

## Supported subscription response types

The wrapper deliberately keeps parsing narrow and predictable:

- plain text containing one share URL per line;
- Base64 encoding of such a list;
- JSON containing share URL strings.

A Clash/Mihomo YAML subscription containing structured proxy objects is **not converted** by this wrapper. Use a provider endpoint that emits share URLs, or add a subscription converter before the supervisor.

## Direct URL compatibility

The old mode still works. Set `UPSTREAM_URL` and leave `SUBSCRIPTION_URL` empty. The supervisor will periodically health-check the single node through the configured probe URL. Override it with `TCP_TEST_URL`; `TEST_URL` remains accepted for backward compatibility.

## Selection semantics

`REVERSE_MATCHES=true` reverses the matched subscription list before testing. The actual winner is still the working node with the lowest HTTP GET round-trip time. `PROBE_ATTEMPTS` controls how many GETs are attempted per candidate; the lowest successful RTT is used, as requested.
