# singbox2proxy subscription supervisor

This image keeps `sing-box` baked into the Docker image and adds a Python supervisor in front of `sb2p`.

## Behaviour

1. Periodically downloads `SUBSCRIPTION_URL` through the healthy active upstream proxy. If no active upstream is healthy, it explicitly fetches the subscription directly.
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
HEALTHCHECK_INTERVAL_SECONDS=60
TCP_TEST_URL=https://www.gstatic.com/generate_204
PROBE_TIMEOUT_SECONDS=8
PROBE_ATTEMPTS=2
PROBE_CONCURRENCY=4
UNAVAILABLE_RETRY_SECONDS=30
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

## Probe startup diagnostics

Probe subprocess output is captured and only emitted when a temporary `sb2p` process fails to start. Proxy/subscription URLs are redacted before logging. Concurrent probe workers also reserve unique local ports so they cannot accidentally collide with each other.

If all nodes fail before the HTTP GET, the log will now contain the underlying `sb2p`/sing-box startup error instead of only `listener did not start`.


## Shadowsocks v2ray-plugin compatibility

Some SIP002 subscriptions encode the plugin and its options in a single query value, for example:

```text
plugin=v2ray-plugin;tls;host=example.com;path=/;mux=0
```

`singbox2proxy` currently passes that complete value to sing-box as the `plugin` field, which makes sing-box reject it as an unknown plugin name. The supervisor detects these Shadowsocks links and launches them directly through sing-box with the correct split:

```json
{
  "plugin": "v2ray-plugin",
  "plugin_opts": "tls;host=example.com;path=/;mux=0"
}
```

sing-box implements its supported SIP003 plugins internally, so an external `v2ray-plugin` executable is not required. Other protocols and ordinary Shadowsocks links continue to use `sb2p`.

## Active proxy health checks and failover

`HEALTHCHECK_INTERVAL_SECONDS` is independent of the full `PROBE_INTERVAL_SECONDS` benchmark. The health check sends the same configured HTTP GET through the currently active proxy only.

If the active proxy process exits, the supervisor immediately uses the most recent successful benchmark ranking and tries the next-lowest RTT candidate, skipping the failed active node. A running proxy must fail `HEALTHCHECK_FAILURE_THRESHOLD` consecutive checks before failover; failed checks are retried after `HEALTHCHECK_RETRY_DELAY_SECONDS`. This avoids switching nodes for a one-off network or TLS failure. It walks the ranking until one activates successfully. If no previously ranked candidate can be activated, it schedules an immediate full benchmark.

Example:

```dotenv
PROBE_INTERVAL_SECONDS=3600
HEALTHCHECK_INTERVAL_SECONDS=30
HEALTHCHECK_FAILURE_THRESHOLD=2
HEALTHCHECK_RETRY_DELAY_SECONDS=2
```

This benchmarks all matching subscription nodes once per hour, but checks the selected active node every 30 seconds. One failed check is retried after two seconds; a second consecutive failure triggers failover without waiting for the next full benchmark.

## No available upstreams

If no subscription candidate works, or a failed active node has no healthy
ranked fallback, the supervisor refreshes the subscription and benchmarks again
after `UNAVAILABLE_RETRY_SECONDS` (30 seconds by default), rather than waiting
for the regular refresh or probe interval. In direct mode, a startup or restart
failure exits the container so Docker's `unless-stopped` policy retries it.

The image health check performs the configured HTTP 204 probe through the
active local HTTP proxy. It reports unhealthy when there is no active listener
or its upstream cannot complete the probe. This makes an unavailable upstream
visible to Docker and orchestration systems.
