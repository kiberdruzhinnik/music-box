#!/usr/bin/env python3
# This process supervisor intentionally turns unexpected parser, subprocess,
# and network failures into logged recovery paths instead of crashing.
# ruff: noqa: BLE001
from __future__ import annotations

import base64
import concurrent.futures
import json
import os
import re
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.parse
import urllib.request
from collections.abc import Iterable
from dataclasses import dataclass
from http.client import HTTPResponse
from types import FrameType
from typing import cast

SUPPORTED_SCHEMES = (
    "vless://", "vmess://", "trojan://", "hysteria2://", "hy2://",
    "hysteria://", "ss://", "tuic://", "wg://", "ssh://",
    "http://", "https://", "socks://", "socks4://", "socks5://",
    "naive+https://",
)

stop_event = threading.Event()
active_process: subprocess.Popen[str] | None = None
active_url: str | None = None
active_name: str | None = None
active_config_path: str | None = None
probe_port_lock = threading.Lock()
probe_ports_in_use: set[int] = set()


def env_int(name: str, default: int, minimum: int = 0) -> int:
    """Read and validate an integer setting from the environment."""
    raw = os.getenv(name, str(default)).strip()
    try:
        value = int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from exc
    if value < minimum:
        raise SystemExit(f"{name} must be >= {minimum}, got {value}")
    return value


def env_bool(name: str, default: bool) -> bool:
    """Read and validate a boolean setting from the environment."""
    raw = os.getenv(name)
    if raw is None:
        return default
    raw = raw.strip().lower()
    if raw in {"1", "true", "yes", "on"}:
        return True
    if raw in {"0", "false", "no", "off"}:
        return False
    raise SystemExit(f"{name} must be true/false, got {raw!r}")


SUBSCRIPTION_URL = os.getenv("SUBSCRIPTION_URL", "").strip()
UPSTREAM_URL = os.getenv("UPSTREAM_URL", "").strip()
COUNTRY = os.getenv("COUNTRY", "").strip()
COUNTRY_REGEX = os.getenv("COUNTRY_REGEX", "").strip()
REVERSE_MATCHES = env_bool("REVERSE_MATCHES", True)
SUBSCRIPTION_REFRESH_SECONDS = env_int("SUBSCRIPTION_REFRESH_SECONDS", 3600, 10)
PROBE_INTERVAL_SECONDS = env_int("PROBE_INTERVAL_SECONDS", 300, 10)
HEALTHCHECK_INTERVAL_SECONDS = env_int("HEALTHCHECK_INTERVAL_SECONDS", 60, 5)
HEALTHCHECK_FAILURE_THRESHOLD = env_int("HEALTHCHECK_FAILURE_THRESHOLD", 2, 1)
HEALTHCHECK_RETRY_DELAY_SECONDS = env_int("HEALTHCHECK_RETRY_DELAY_SECONDS", 2, 1)
PROBE_TIMEOUT_SECONDS = env_int("PROBE_TIMEOUT_SECONDS", 8, 1)
PROBE_ATTEMPTS = env_int("PROBE_ATTEMPTS", 2, 1)
PROBE_CONCURRENCY = env_int("PROBE_CONCURRENCY", 4, 1)
MAX_CANDIDATES = env_int("MAX_CANDIDATES", 0, 0)
UNAVAILABLE_RETRY_SECONDS = env_int("UNAVAILABLE_RETRY_SECONDS", 30, 5)
TEST_URL = os.getenv("TCP_TEST_URL", os.getenv("TEST_URL", "https://www.gstatic.com/generate_204")).strip()
SUBSCRIPTION_USER_AGENT = os.getenv("SUBSCRIPTION_USER_AGENT", "singbox2proxy-supervisor/1.0").strip()
SB2P_INTERNAL_SOCKS_PORT = env_int("SB2P_INTERNAL_SOCKS_PORT", 11080, 1)
SB2P_INTERNAL_HTTP_PORT = env_int("SB2P_INTERNAL_HTTP_PORT", 18080, 1)


@dataclass(frozen=True)
class Candidate:
    index: int
    url: str
    name: str


@dataclass(frozen=True)
class ProbeResult:
    candidate: Candidate
    ok: bool
    rtt_ms: float | None
    detail: str


def log(msg: str) -> None:
    """Write a timestamped supervisor message to standard output."""
    stamp = time.strftime("%Y-%m-%d %H:%M:%S")
    print(f"{stamp} [supervisor] {msg}", flush=True)


def on_signal(signum: int, _frame: FrameType | None) -> None:
    """Stop the active proxy and request shutdown on SIGTERM or SIGINT."""
    log(f"received signal {signum}; stopping")
    stop_event.set()
    terminate_active()


_ = signal.signal(signal.SIGTERM, on_signal)
_ = signal.signal(signal.SIGINT, on_signal)


def terminate_process(proc: subprocess.Popen[str] | None, timeout: float = 3.0) -> None:
    """Terminate a child process, killing it if graceful shutdown times out."""
    if proc is None or proc.poll() is not None:
        return
    proc.terminate()
    try:
        _ = proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        proc.kill()
        try:
            _ = proc.wait(timeout=1)
        except subprocess.TimeoutExpired:
            pass


def terminate_active() -> None:
    """Stop the active proxy and remove its temporary configuration."""
    global active_process, active_url, active_name, active_config_path
    proc = active_process
    config_path = active_config_path
    active_process = None
    active_url = None
    active_name = None
    active_config_path = None
    terminate_process(proc)
    if config_path:
        try:
            os.unlink(config_path)
        except FileNotFoundError:
            pass


def is_proxy_url(text: str) -> bool:
    """Return whether text starts with a supported proxy share scheme."""
    s = text.strip()
    return any(s.lower().startswith(prefix) for prefix in SUPPORTED_SCHEMES)


def add_padding(value: str) -> str:
    """Pad a Base64 value to a valid four-character boundary."""
    return value + "=" * (-len(value) % 4)


def maybe_b64decode_text(value: str) -> str | None:
    """Decode Base64 text only when it appears to contain proxy links."""
    compact = "".join(value.split())
    if not compact:
        return None
    try:
        raw = base64.urlsafe_b64decode(add_padding(compact))
        decoded = raw.decode("utf-8")
    except Exception:
        return None
    if any(prefix in decoded.lower() for prefix in SUPPORTED_SCHEMES):
        return decoded
    return None


def collect_urls_from_json(obj: object) -> list[str]:
    """Recursively collect supported share URLs from JSON values."""
    found: list[str] = []
    if isinstance(obj, str):
        if is_proxy_url(obj):
            found.append(obj.strip())
    elif isinstance(obj, list):
        for item in cast(list[object], obj):
            found.extend(collect_urls_from_json(item))
    elif isinstance(obj, dict):
        for value in cast(dict[str, object], obj).values():
            found.extend(collect_urls_from_json(value))
    return found


def parse_subscription(body: bytes) -> list[str]:
    """Extract unique share URLs from plain, JSON, or Base64 subscriptions."""
    text = body.decode("utf-8", errors="replace").strip().lstrip("\ufeff")
    found: list[str] = []

    # Direct/plain URI subscription.
    for line in text.splitlines():
        line = line.strip()
        if line and not line.startswith("#") and is_proxy_url(line):
            found.append(line)

    # JSON containing URI strings.
    if not found and text[:1] in "[{":
        try:
            found.extend(collect_urls_from_json(cast(object, json.loads(text))))
        except json.JSONDecodeError:
            pass

    # Classic Base64-encoded URI subscription.
    if not found:
        decoded = maybe_b64decode_text(text)
        if decoded:
            for line in decoded.splitlines():
                line = line.strip()
                if line and not line.startswith("#") and is_proxy_url(line):
                    found.append(line)

    # Stable de-duplication.
    seen: set[str] = set()
    unique: list[str] = []
    for url in found:
        if url not in seen:
            seen.add(url)
            unique.append(url)
    return unique


def active_subscription_proxy() -> str | None:
    """Return the local HTTP proxy only while its upstream is healthy."""
    if not active_proxy_is_running():
        return None
    try:
        rtt = verify_active()
        log(f"active upstream is available for subscription refresh; RTT {rtt:.1f} ms")
    except Exception as exc:
        log(f"active upstream is unavailable for subscription refresh: {exc}")
        return None
    return f"http://127.0.0.1:{SB2P_INTERNAL_HTTP_PORT}"


def fetch_subscription() -> list[str]:
    """Fetch and parse the subscription through a live proxy or directly."""
    request = urllib.request.Request(
        SUBSCRIPTION_URL,
        headers={
            "User-Agent": SUBSCRIPTION_USER_AGENT,
            "Accept": "*/*",
            "Cache-Control": "no-cache",
        },
    )
    proxy_url = active_subscription_proxy()
    if proxy_url:
        log("fetching subscription through the healthy active upstream proxy")
        opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({"http": proxy_url, "https": proxy_url})
        )
    else:
        log("no healthy active upstream proxy; fetching subscription directly")
        # An empty ProxyHandler makes the direct fallback independent of any
        # proxy-related environment variables inherited by the container.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with cast(HTTPResponse, opener.open(request, timeout=PROBE_TIMEOUT_SECONDS + 5)) as response:
        body = response.read()
    urls = parse_subscription(body)
    if not urls:
        raise RuntimeError(
            "subscription contained no supported share URLs; this wrapper currently supports "
            + "plain URI lists, Base64 URI lists, and JSON containing URI strings"
        )
    return urls


def vmess_name(url: str) -> str | None:
    """Read a VMess node name from its encoded JSON payload."""
    payload = url[len("vmess://"):].split("#", 1)[0].strip()
    try:
        decoded = base64.urlsafe_b64decode(add_padding(payload)).decode("utf-8")
        data = cast(dict[str, object], json.loads(decoded))
        name = data.get("ps")
        return str(name) if name else None
    except Exception:
        return None


def candidate_name(url: str) -> str:
    """Derive a display name from a share URL without exposing credentials."""
    if url.lower().startswith("vmess://"):
        name = vmess_name(url)
        if name:
            return name
    try:
        parsed = urllib.parse.urlsplit(url)
        if parsed.fragment:
            return urllib.parse.unquote(parsed.fragment)
        if parsed.hostname:
            return parsed.hostname
    except Exception:
        return "unnamed"
    return "unnamed"


def filter_candidates(urls: Iterable[str]) -> list[Candidate]:
    """Apply name filters, ordering, and the candidate limit."""
    candidates = [Candidate(i, url, candidate_name(url)) for i, url in enumerate(urls)]

    if COUNTRY_REGEX:
        try:
            rx = re.compile(COUNTRY_REGEX, re.IGNORECASE)
        except re.error as exc:
            raise RuntimeError(f"invalid COUNTRY_REGEX: {exc}") from exc
        candidates = [c for c in candidates if rx.search(c.name)]
    elif COUNTRY:
        needle = COUNTRY.casefold()
        candidates = [c for c in candidates if needle in c.name.casefold()]

    if REVERSE_MATCHES:
        candidates.reverse()

    if MAX_CANDIDATES:
        candidates = candidates[:MAX_CANDIDATES]

    if not candidates:
        selector = f"COUNTRY_REGEX={COUNTRY_REGEX!r}" if COUNTRY_REGEX else f"COUNTRY={COUNTRY!r}"
        raise RuntimeError(f"no subscription nodes matched {selector}")
    return candidates


def reserve_probe_port() -> int:
    """Reserve a distinct loopback port for a temporary probe listener."""
    # Avoid a race where multiple concurrent workers receive the same ephemeral
    # port between the bind(0) discovery step and sb2p actually binding it.
    while True:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.bind(("127.0.0.1", 0))
            port = cast(tuple[str, int], sock.getsockname())[1]
        with probe_port_lock:
            if port not in probe_ports_in_use:
                probe_ports_in_use.add(port)
                return port


def release_probe_port(port: int) -> None:
    """Release a probe port reservation after its child process exits."""
    with probe_port_lock:
        probe_ports_in_use.discard(port)




def _decode_ss_userinfo(value: str) -> tuple[str, str]:
    """Decode SIP002 Shadowsocks method and password userinfo."""
    raw = urllib.parse.unquote(value)
    # SIP002 uses URL-safe base64(method:password) in the userinfo portion.
    try:
        decoded = base64.urlsafe_b64decode(add_padding(raw)).decode("utf-8")
    except Exception:
        decoded = raw
    if ":" not in decoded:
        raise ValueError("invalid Shadowsocks credentials; expected method:password")
    method, password = decoded.split(":", 1)
    return method, password


def parse_shadowsocks_url(url: str) -> dict[str, str | int] | None:
    """Convert a Shadowsocks share URL into a sing-box outbound."""
    if not url.lower().startswith("ss://"):
        return None

    parsed = urllib.parse.urlsplit(url)
    host = parsed.hostname
    port = parsed.port
    method = ""
    password = ""

    if host and port:
        if parsed.password is not None:
            method = urllib.parse.unquote(parsed.username or "")
            password = urllib.parse.unquote(parsed.password)
        elif parsed.username:
            method, password = _decode_ss_userinfo(parsed.username)
    else:
        # Legacy form: ss://BASE64(method:password@host:port)#name
        payload = url[len("ss://"):].split("#", 1)[0].split("?", 1)[0].rstrip("/")
        decoded = base64.urlsafe_b64decode(add_padding(payload)).decode("utf-8")
        creds, endpoint = decoded.rsplit("@", 1)
        method, password = creds.split(":", 1)
        if endpoint.startswith("["):
            end = endpoint.rfind("]")
            host = endpoint[1:end]
            port = int(endpoint[end + 2:])
        else:
            host, port_text = endpoint.rsplit(":", 1)
            port = int(port_text)

    if not host or not port or not method:
        raise ValueError("invalid Shadowsocks share URL")

    query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
    plugin_raw = query.get("plugin", [""])[0]
    plugin = ""
    plugin_opts = ""
    if plugin_raw:
        # SIP002 encodes plugin name and options in one query value:
        # plugin=v2ray-plugin;tls;host=example.com;path=/;mux=0
        plugin, sep, plugin_opts = plugin_raw.partition(";")
        if not sep:
            plugin_opts = query.get("plugin_opts", query.get("plugin-opts", [""]))[0]

    outbound: dict[str, str | int] = {
        "type": "shadowsocks",
        "tag": "proxy",
        "server": host,
        "server_port": int(port),
        "method": method,
        "password": password,
    }
    if plugin:
        outbound["plugin"] = plugin
    if plugin_opts:
        outbound["plugin_opts"] = plugin_opts
    return outbound


def needs_direct_shadowsocks(url: str) -> bool:
    """Identify SIP002 plugin links that require direct sing-box startup."""
    if not url.lower().startswith("ss://"):
        return False
    try:
        parsed = urllib.parse.urlsplit(url)
        plugin_raw = urllib.parse.parse_qs(parsed.query, keep_blank_values=True).get("plugin", [""])[0]
        # sb2p currently passes this entire SIP002 value as sing-box's plugin name.
        return ";" in plugin_raw
    except Exception:
        return False


def write_direct_ss_config(url: str, http_port: int, socks_port: int) -> str:
    """Write a temporary sing-box configuration for a Shadowsocks node."""
    outbound = parse_shadowsocks_url(url)
    if outbound is None:
        raise ValueError("not a Shadowsocks URL")
    config = {
        "log": {"level": "error"},
        "inbounds": [
            {"type": "http", "tag": "http-in", "listen": "127.0.0.1", "listen_port": http_port},
            {"type": "socks", "tag": "socks-in", "listen": "127.0.0.1", "listen_port": socks_port},
        ],
        "outbounds": [outbound],
        "route": {"final": "proxy"},
    }
    fd, path = tempfile.mkstemp(prefix="sb2p-ss-", suffix=".json")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(config, f, separators=(",", ":"))
    except Exception:
        os.close(fd)
        raise
    return path


def launch_proxy_process(candidate: Candidate, http_port: int, socks_port: int, quiet: bool) -> tuple[subprocess.Popen[str], str | None]:
    """Start the proxy process and return its optional temporary config path."""
    config_path: str | None = None
    if needs_direct_shadowsocks(candidate.url):
        config_path = write_direct_ss_config(candidate.url, http_port, socks_port)
        command = ["sing-box", "run", "-c", config_path]
    else:
        command = [
            "sb2p", candidate.url,
            "--http-port", str(http_port),
            "--socks-port", str(socks_port),
        ]
        if quiet:
            command.append("--quiet")

    proc = subprocess.Popen(
        command,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE if quiet else None,
        stderr=subprocess.PIPE if quiet else None,
        text=True,
    )
    return proc, config_path


def redact_process_output(text: str, candidate: Candidate) -> str:
    """Remove share and subscription URLs from child process diagnostics."""
    if not text:
        return ""
    # sb2p/sing-box errors can echo the share URL, which may contain credentials.
    redacted = text.replace(candidate.url, "<proxy-url-redacted>")
    redacted = redacted.replace(SUBSCRIPTION_URL, "<subscription-url-redacted>") if SUBSCRIPTION_URL else redacted
    # Collapse multiline output to keep per-candidate logs readable.
    return " | ".join(line.strip() for line in redacted.splitlines() if line.strip())[-1200:]

def process_start_error(proc: subprocess.Popen[str], candidate: Candidate) -> str:
    """Describe a failed proxy startup using redacted process output."""
    if proc.poll() is None:
        return "proxy listener did not start before timeout"
    try:
        stdout, stderr = proc.communicate(timeout=0.5)
    except Exception:
        stdout, stderr = "", ""
    detail = redact_process_output((stderr or "") + "\n" + (stdout or ""), candidate)
    if detail:
        return f"proxy process exited with status {proc.returncode}: {detail}"
    return f"proxy process exited with status {proc.returncode} before listener became ready"


def wait_for_port(port: int, proc: subprocess.Popen[str], timeout: float) -> bool:
    """Wait for a child process to open its loopback listener."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            return False
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.15):
                return True
        except OSError:
            time.sleep(0.05)
    return False


def http_probe_via_proxy(proxy_port: int) -> float:
    """Measure an HTTP 204 request through the local proxy listener."""
    proxy_url = f"http://127.0.0.1:{proxy_port}"
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({"http": proxy_url, "https": proxy_url})
    )
    request = urllib.request.Request(
        TEST_URL,
        headers={
            "User-Agent": "sb2p-health-probe/1.0",
            "Cache-Control": "no-cache",
            "Connection": "close",
        },
        method="GET",
    )
    started = time.perf_counter()
    with cast(HTTPResponse, opener.open(request, timeout=PROBE_TIMEOUT_SECONDS)) as response:
        status = response.status
        _ = response.read(1)
    elapsed_ms = (time.perf_counter() - started) * 1000.0
    if status != 204:
        raise RuntimeError(f"HTTP {status}, expected 204")
    return elapsed_ms


def probe_candidate(candidate: Candidate) -> ProbeResult:
    """Start and benchmark one node, then clean up its temporary resources."""
    # This sb2p build requires integer values for both --http-port and
    # --socks-port. Give each probe its own pair of reserved ports even
    # though the health check itself only uses the HTTP listener.
    http_port = reserve_probe_port()
    socks_port = reserve_probe_port()
    proc: subprocess.Popen[str] | None = None
    config_path: str | None = None
    try:
        proc, config_path = launch_proxy_process(candidate, http_port, socks_port, quiet=True)
        if not wait_for_port(http_port, proc, min(5.0, float(PROBE_TIMEOUT_SECONDS))):
            return ProbeResult(candidate, False, None, process_start_error(proc, candidate))

        samples: list[float] = []
        for _ in range(PROBE_ATTEMPTS):
            try:
                samples.append(http_probe_via_proxy(http_port))
            except Exception as exc:
                if not samples:
                    return ProbeResult(candidate, False, None, str(exc))
                break

        # User requested the lowest observed round-trip time.
        best_ms = min(samples)
        return ProbeResult(candidate, True, best_ms, f"samples={','.join(f'{x:.1f}' for x in samples)}")
    except Exception as exc:
        return ProbeResult(candidate, False, None, str(exc))
    finally:
        terminate_process(proc, timeout=1.5)
        if config_path:
            try:
                os.unlink(config_path)
            except FileNotFoundError:
                pass
        release_probe_port(http_port)
        release_probe_port(socks_port)


def benchmark(candidates: list[Candidate]) -> list[ProbeResult]:
    """Rank live candidates by RTT and log the alive/filtered count."""
    log(
        f"probing {len(candidates)} candidate(s) against {TEST_URL} "
        + f"with concurrency={PROBE_CONCURRENCY}, attempts={PROBE_ATTEMPTS}"
    )
    results: list[ProbeResult] = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=PROBE_CONCURRENCY) as pool:
        future_map = {pool.submit(probe_candidate, c): c for c in candidates}
        for future in concurrent.futures.as_completed(future_map):
            result = future.result()
            results.append(result)
            if result.ok:
                log(f"probe OK   {result.rtt_ms:7.1f} ms  {result.candidate.name}")
            else:
                log(f"probe FAIL             {result.candidate.name}: {result.detail}")

    working = [r for r in results if r.ok and r.rtt_ms is not None]
    working.sort(key=lambda r: (r.rtt_ms, -r.candidate.index))
    log(f"retest summary: filtered={len(candidates)}, alive={len(working)}/{len(candidates)}")
    return working


def start_active(candidate: Candidate) -> tuple[subprocess.Popen[str], str | None]:
    """Start a candidate on the stable internal listener ports."""
    proc, config_path = launch_proxy_process(
        candidate, SB2P_INTERNAL_HTTP_PORT, SB2P_INTERNAL_SOCKS_PORT, quiet=False
    )
    if not wait_for_port(SB2P_INTERNAL_HTTP_PORT, proc, 8.0):
        terminate_process(proc)
        if config_path:
            try:
                os.unlink(config_path)
            except FileNotFoundError:
                pass
        raise RuntimeError("active proxy HTTP listener did not start")
    return proc, config_path


def verify_active() -> float:
    """Measure the active proxy using the configured HTTP probe."""
    return http_probe_via_proxy(SB2P_INTERNAL_HTTP_PORT)


def active_proxy_is_running() -> bool:
    """Return whether the active proxy child process is still alive."""
    return active_process is not None and active_process.poll() is None


def schedule_unavailable_retry() -> tuple[float, float]:
    """Return refresh/probe deadlines while no usable active proxy exists."""
    retry_at = time.monotonic() + UNAVAILABLE_RETRY_SECONDS
    return retry_at, retry_at


def schedule_healthcheck_failure(failures: int) -> tuple[int, float, bool]:
    """Track a failed health check and decide whether to fail over."""
    failures += 1
    if failures < HEALTHCHECK_FAILURE_THRESHOLD:
        return failures, time.monotonic() + HEALTHCHECK_RETRY_DELAY_SECONDS, False
    return failures, time.monotonic() + HEALTHCHECK_INTERVAL_SECONDS, True


def activate(candidate: Candidate) -> bool:
    """Keep or replace the active proxy and verify it before acceptance."""
    global active_process, active_url, active_name, active_config_path

    if active_url == candidate.url and active_process is not None and active_process.poll() is None:
        try:
            rtt = verify_active()
            log(f"keeping active node {candidate.name}; live probe {rtt:.1f} ms")
            return True
        except Exception as exc:
            log(f"active node {candidate.name} failed live probe: {exc}; restarting")

    old = active_process
    old_config = active_config_path
    active_process = None
    active_url = None
    active_name = None
    active_config_path = None
    terminate_process(old)
    if old_config:
        try:
            os.unlink(old_config)
        except FileNotFoundError:
            pass

    try:
        proc, config_path = start_active(candidate)
        active_process = proc
        active_config_path = config_path
        active_url = candidate.url
        active_name = candidate.name
        rtt = verify_active()
        log(f"ACTIVE {candidate.name}; verification RTT {rtt:.1f} ms")
        return True
    except Exception as exc:
        log(f"failed to activate {candidate.name}: {exc}")
        terminate_process(active_process)
        if active_config_path:
            try:
                os.unlink(active_config_path)
            except FileNotFoundError:
                pass
        active_process = None
        active_url = None
        active_name = None
        active_config_path = None
        return False


def choose_and_activate(working: list[ProbeResult]) -> None:
    """Activate the fastest working candidate with ranked fallbacks."""
    if not working:
        log("no working candidates; keeping current active node if it is still running")
        return

    best = working[0]
    log(f"best measured node: {best.candidate.name} at {best.rtt_ms:.1f} ms")

    # Try candidates in measured order. If the fastest fails when moved to the
    # stable active ports, fall back to the next-fastest working node.
    for result in working:
        if activate(result.candidate):
            return
    log("all benchmarked candidates failed activation")


def failover_from_ranking(working: list[ProbeResult], failed_url: str | None) -> bool:
    """Try ranked alternatives while skipping the failed active node."""
    if not working:
        return False

    for result in working:
        if failed_url and result.candidate.url == failed_url:
            continue
        log(f"failover candidate: {result.candidate.name} at last RTT {result.rtt_ms:.1f} ms")
        if activate(result.candidate):
            return True
    return False


def direct_mode() -> int:
    """Supervise a single configured upstream and restart it on failure."""
    candidate = Candidate(0, UPSTREAM_URL, candidate_name(UPSTREAM_URL))
    log(f"direct mode; starting {candidate.name}")
    if not activate(candidate):
        return 1
    while not stop_event.wait(HEALTHCHECK_INTERVAL_SECONDS):
        if active_process is None or active_process.poll() is not None:
            log("active proxy exited; restarting")
            _ = activate(candidate)
            continue
        try:
            rtt = verify_active()
            log(f"direct node healthy; RTT {rtt:.1f} ms")
        except Exception as exc:
            log(f"direct node health check failed: {exc}; restarting")
            _ = activate(candidate)
    return 0


def subscription_mode() -> int:
    """Refresh, benchmark, select, and monitor subscription proxies."""
    urls: list[str] = []
    candidates: list[Candidate] = []
    working_ranking: list[ProbeResult] = []
    next_refresh = 0.0
    next_probe = 0.0
    next_healthcheck = 0.0
    healthcheck_failures = 0

    while not stop_event.is_set():
        now = time.monotonic()

        if now >= next_refresh:
            refresh_delay = SUBSCRIPTION_REFRESH_SECONDS
            try:
                fetched = fetch_subscription()
                filtered = filter_candidates(fetched)
                urls = fetched
                candidates = filtered
                log(
                    f"subscription refreshed: {len(urls)} total node(s), "
                    + f"{len(candidates)} matched; reverse={REVERSE_MATCHES}"
                )
                for pos, candidate in enumerate(candidates, 1):
                    log(f"candidate {pos:02d}: {candidate.name}")
                next_probe = 0.0
            except Exception as exc:
                log(f"subscription refresh failed: {exc}")
                if not active_proxy_is_running():
                    refresh_delay = UNAVAILABLE_RETRY_SECONDS
                    log(
                        "no active upstream proxy is available; retrying subscription refresh "
                        + f"in {UNAVAILABLE_RETRY_SECONDS}s"
                    )
            finally:
                next_refresh = time.monotonic() + refresh_delay

        now = time.monotonic()
        if candidates and now >= next_probe:
            try:
                working_ranking = benchmark(candidates)
                choose_and_activate(working_ranking)
                if not working_ranking and not active_proxy_is_running():
                    next_refresh, next_probe = schedule_unavailable_retry()
                    log(
                        "no upstream proxy is available; retrying subscription refresh and "
                        + f"benchmark in {UNAVAILABLE_RETRY_SECONDS}s"
                    )
                    continue
                next_healthcheck = time.monotonic() + HEALTHCHECK_INTERVAL_SECONDS
            except Exception as exc:
                log(f"benchmark cycle failed: {exc}")
            finally:
                if next_probe <= time.monotonic():
                    next_probe = time.monotonic() + PROBE_INTERVAL_SECONDS

        now = time.monotonic()
        if active_process is not None and active_process.poll() is not None:
            failed_url = active_url
            failed_name = active_name or "active node"
            log(f"{failed_name} process exited with status {active_process.returncode}; trying next ranked node")
            healthcheck_failures = 0
            if not failover_from_ranking(working_ranking, failed_url):
                log("no ranked failover candidate succeeded; scheduling unavailable retry")
                next_refresh, next_probe = schedule_unavailable_retry()
            next_healthcheck = time.monotonic() + HEALTHCHECK_INTERVAL_SECONDS

        now = time.monotonic()
        if active_process is not None and active_process.poll() is None and now >= next_healthcheck:
            failed_url = active_url
            failed_name = active_name or "active node"
            try:
                rtt = verify_active()
                log(f"active healthcheck OK   {rtt:.1f} ms  {failed_name}")
                healthcheck_failures = 0
                next_healthcheck = time.monotonic() + HEALTHCHECK_INTERVAL_SECONDS
            except Exception as exc:
                healthcheck_failures, next_healthcheck, should_fail_over = schedule_healthcheck_failure(
                    healthcheck_failures
                )
                if not should_fail_over:
                    log(
                        f"active healthcheck FAIL          {failed_name}: {exc}; "
                        + f"retrying in {HEALTHCHECK_RETRY_DELAY_SECONDS}s "
                        + f"({healthcheck_failures}/{HEALTHCHECK_FAILURE_THRESHOLD})"
                    )
                else:
                    log(
                        f"active healthcheck FAIL          {failed_name}: {exc}; "
                        + f"failure threshold ({healthcheck_failures}/{HEALTHCHECK_FAILURE_THRESHOLD}) reached"
                    )
                    healthcheck_failures = 0
                    if not failover_from_ranking(working_ranking, failed_url):
                        log("no ranked failover candidate succeeded; scheduling unavailable retry")
                        next_refresh, next_probe = schedule_unavailable_retry()

        wake_times = [next_refresh]
        if candidates:
            wake_times.append(next_probe)
        if active_process is not None:
            wake_times.append(next_healthcheck)
        wake_at = min(wake_times)
        sleep_for = max(0.2, min(2.0, wake_at - time.monotonic()))
        _ = stop_event.wait(sleep_for)

    return 0

def main() -> int:
    """Validate the configured mode and start its supervisor loop."""
    if bool(SUBSCRIPTION_URL) == bool(UPSTREAM_URL):
        log("set exactly one of SUBSCRIPTION_URL or UPSTREAM_URL")
        return 2

    log(
        f"probe target={TEST_URL}; subscription_refresh={SUBSCRIPTION_REFRESH_SECONDS}s; "
        + f"probe_interval={PROBE_INTERVAL_SECONDS}s; healthcheck_interval={HEALTHCHECK_INTERVAL_SECONDS}s"
    )
    if SUBSCRIPTION_URL:
        return subscription_mode()
    return direct_mode()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    finally:
        terminate_active()
