"""Behavioral tests for supervisor recovery paths.

These tests deliberately mock proxy processes and HTTP probes. They must not
load `.env`, contact a subscription provider, or require Docker.
"""

from __future__ import annotations

import pathlib
import subprocess
import sys
import threading
import time
import unittest
from typing import cast, override
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import supervisor


class FakeProcess:
    def __init__(self, returncode: int | None = None) -> None:
        """Create a fake process with the requested exit state."""
        self.returncode: int | None = returncode

    def poll(self) -> int | None:
        """Return the fake process exit status."""
        return self.returncode


class StopAfterWaits:
    def __init__(self, waits: int) -> None:
        """Allow a fixed number of loop waits before signaling stop."""
        self.remaining: int = waits

    def wait(self, _timeout: float) -> bool:
        """Signal stop after the configured number of waits."""
        if self.remaining:
            self.remaining -= 1
            return False
        return True


def as_active_process(fake: FakeProcess) -> subprocess.Popen[str]:
    """Treat the minimal process double as an active child in tests."""
    return cast(subprocess.Popen[str], cast(object, fake))


def as_stop_event(fake: StopAfterWaits) -> threading.Event:
    """Treat the bounded wait double as the supervisor stop event."""
    return cast(threading.Event, cast(object, fake))


class SupervisorStateTest(unittest.TestCase):
    state: dict[str, object] | None = None

    @override
    def setUp(self) -> None:
        """Save mutable supervisor globals before each test."""
        self.state = {
            name: getattr(supervisor, name)
            for name in (
                "UPSTREAM_URL",
                "active_process",
                "active_url",
                "active_name",
                "active_config_path",
                "stop_event",
            )
        }

    @override
    def tearDown(self) -> None:
        """Restore mutable supervisor globals after each test."""
        assert self.state is not None
        for name, value in self.state.items():
            setattr(supervisor, name, value)

    def test_direct_mode_restarts_after_active_process_exits(self) -> None:
        """Restart the configured direct node after its process exits."""
        supervisor.UPSTREAM_URL = "vless://fake-node#test"
        supervisor.active_process = as_active_process(FakeProcess(returncode=17))
        supervisor.stop_event = as_stop_event(StopAfterWaits(1))

        with mock.patch.object(supervisor, "activate", return_value=True) as activate:
            self.assertEqual(supervisor.direct_mode(), 0)

        self.assertEqual(activate.call_count, 2)
        selected = cast(supervisor.Candidate, activate.call_args_list[0].args[0])
        self.assertEqual(selected.url, supervisor.UPSTREAM_URL)

    def test_direct_mode_restarts_after_unavailable_upstream_health_check(self) -> None:
        """Restart a direct node when its active probe fails."""
        supervisor.UPSTREAM_URL = "trojan://fake-node#test"
        supervisor.active_process = as_active_process(FakeProcess())
        supervisor.stop_event = as_stop_event(StopAfterWaits(1))

        with (
            mock.patch.object(supervisor, "activate", return_value=True) as activate,
            mock.patch.object(supervisor, "verify_active", side_effect=OSError("upstream unavailable")),
        ):
            self.assertEqual(supervisor.direct_mode(), 0)

        self.assertEqual(activate.call_count, 2)

    def test_failed_active_verification_clears_the_failed_process(self) -> None:
        """Discard a newly started active process that fails verification."""
        candidate = supervisor.Candidate(0, "ss://fake-node#test", "test")
        new_process = FakeProcess()
        supervisor.active_process = None
        supervisor.active_url = None
        supervisor.active_name = None
        supervisor.active_config_path = None

        with (
            mock.patch.object(supervisor, "start_active", return_value=(new_process, None)),
            mock.patch.object(supervisor, "verify_active", side_effect=OSError("upstream unavailable")),
            mock.patch.object(supervisor, "terminate_process") as terminate,
        ):
            self.assertFalse(supervisor.activate(candidate))

        self.assertEqual(terminate.call_args_list, [mock.call(None), mock.call(new_process)])
        self.assertIsNone(supervisor.active_process)
        self.assertIsNone(supervisor.active_url)
        self.assertIsNone(supervisor.active_name)

    def test_failover_skips_failed_node_and_uses_next_ranked_node(self) -> None:
        """Skip the failed node when activating a ranked fallback."""
        failed = supervisor.Candidate(0, "vless://failed#failed", "failed")
        fallback = supervisor.Candidate(1, "vless://fallback#fallback", "fallback")
        ranking = [
            supervisor.ProbeResult(failed, True, 10.0, ""),
            supervisor.ProbeResult(fallback, True, 20.0, ""),
        ]

        with mock.patch.object(supervisor, "activate", return_value=True) as activate:
            self.assertTrue(supervisor.failover_from_ranking(ranking, failed.url))

        activate.assert_called_once_with(fallback)

    def test_benchmark_logs_alive_over_filtered_summary(self) -> None:
        """Report live and filtered node counts after every benchmark."""
        first = supervisor.Candidate(0, "vless://first#first", "first")
        second = supervisor.Candidate(1, "vless://second#second", "second")
        candidates = [first, second]

        for first_alive, expected in ((True, "alive=1/2"), (False, "alive=0/2")):
            with self.subTest(first_alive=first_alive):
                def probe(candidate: supervisor.Candidate, alive: bool = first_alive) -> supervisor.ProbeResult:
                    """Return a fake probe result for the current subtest."""
                    if candidate == first and alive:
                        return supervisor.ProbeResult(candidate, True, 10.0, "")
                    return supervisor.ProbeResult(candidate, False, None, "unavailable")

                with (
                    mock.patch.object(supervisor, "probe_candidate", side_effect=probe),
                    mock.patch.object(supervisor, "log") as log,
                ):
                    working = supervisor.benchmark(candidates)

                self.assertEqual(len(working), int(first_alive))
                log.assert_any_call(f"retest summary: filtered=2, {expected}")

    def test_unavailable_retry_uses_short_cadence_for_refresh_and_probe(self) -> None:
        """Schedule both refresh and probe promptly when no upstream works."""
        original_retry = supervisor.UNAVAILABLE_RETRY_SECONDS
        supervisor.UNAVAILABLE_RETRY_SECONDS = 37
        try:
            with mock.patch.object(time, "monotonic", return_value=100.0):
                refresh_at, probe_at = supervisor.schedule_unavailable_retry()
        finally:
            supervisor.UNAVAILABLE_RETRY_SECONDS = original_retry

        self.assertEqual(refresh_at, 137.0)
        self.assertEqual(probe_at, 137.0)

    def test_healthcheck_failure_retries_before_failover(self) -> None:
        """Retry one failed health check before reaching the failover threshold."""
        original_threshold = supervisor.HEALTHCHECK_FAILURE_THRESHOLD
        original_delay = supervisor.HEALTHCHECK_RETRY_DELAY_SECONDS
        original_interval = supervisor.HEALTHCHECK_INTERVAL_SECONDS
        supervisor.HEALTHCHECK_FAILURE_THRESHOLD = 2
        supervisor.HEALTHCHECK_RETRY_DELAY_SECONDS = 2
        supervisor.HEALTHCHECK_INTERVAL_SECONDS = 60
        try:
            with mock.patch.object(time, "monotonic", return_value=100.0):
                failures, retry_at, should_fail_over = supervisor.schedule_healthcheck_failure(0)
                self.assertEqual((failures, retry_at, should_fail_over), (1, 102.0, False))

                failures, next_check_at, should_fail_over = supervisor.schedule_healthcheck_failure(failures)
                self.assertEqual((failures, next_check_at, should_fail_over), (2, 160.0, True))
        finally:
            supervisor.HEALTHCHECK_FAILURE_THRESHOLD = original_threshold
            supervisor.HEALTHCHECK_RETRY_DELAY_SECONDS = original_delay
            supervisor.HEALTHCHECK_INTERVAL_SECONDS = original_interval

    def test_subscription_refresh_uses_healthy_active_upstream_proxy(self) -> None:
        """Fetch through the active proxy when its probe passes."""
        supervisor.active_process = as_active_process(FakeProcess())
        with mock.patch.object(supervisor, "verify_active", return_value=12.5):
            self.assertEqual(
                supervisor.active_subscription_proxy(),
                f"http://127.0.0.1:{supervisor.SB2P_INTERNAL_HTTP_PORT}",
            )

    def test_subscription_refresh_falls_back_to_direct_when_active_upstream_is_unhealthy(self) -> None:
        """Use a direct fetch when the active upstream fails its probe."""
        supervisor.active_process = as_active_process(FakeProcess())
        with mock.patch.object(supervisor, "verify_active", side_effect=OSError("unavailable")):
            self.assertIsNone(supervisor.active_subscription_proxy())

    def test_subscription_refresh_uses_no_proxy_when_no_active_upstream_exists(self) -> None:
        """Use a direct fetch without probing when no active process exists."""
        supervisor.active_process = None
        with mock.patch.object(supervisor, "verify_active") as verify:
            self.assertIsNone(supervisor.active_subscription_proxy())
        verify.assert_not_called()


if __name__ == "__main__":
    _ = unittest.main()
