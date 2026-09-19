"""Behavioral tests for supervisor recovery paths.

These tests deliberately mock proxy processes and HTTP probes. They must not
load `.env`, contact a subscription provider, or require Docker.
"""

from __future__ import annotations

import pathlib
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import supervisor  # noqa: E402


class FakeProcess:
    def __init__(self, returncode: int | None = None) -> None:
        self.returncode = returncode

    def poll(self) -> int | None:
        return self.returncode


class StopAfterWaits:
    def __init__(self, waits: int) -> None:
        self.remaining = waits

    def wait(self, _timeout: float) -> bool:
        if self.remaining:
            self.remaining -= 1
            return False
        return True


class SupervisorStateTest(unittest.TestCase):
    def setUp(self) -> None:
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

    def tearDown(self) -> None:
        for name, value in self.state.items():
            setattr(supervisor, name, value)

    def test_direct_mode_restarts_after_active_process_exits(self) -> None:
        supervisor.UPSTREAM_URL = "vless://fake-node#test"
        supervisor.active_process = FakeProcess(returncode=17)
        supervisor.stop_event = StopAfterWaits(1)

        with mock.patch.object(supervisor, "activate", return_value=True) as activate:
            self.assertEqual(supervisor.direct_mode(), 0)

        self.assertEqual(activate.call_count, 2)
        self.assertEqual(activate.call_args_list[0].args[0].url, supervisor.UPSTREAM_URL)

    def test_direct_mode_restarts_after_unavailable_upstream_health_check(self) -> None:
        supervisor.UPSTREAM_URL = "trojan://fake-node#test"
        supervisor.active_process = FakeProcess()
        supervisor.stop_event = StopAfterWaits(1)

        with (
            mock.patch.object(supervisor, "activate", return_value=True) as activate,
            mock.patch.object(supervisor, "verify_active", side_effect=OSError("upstream unavailable")),
        ):
            self.assertEqual(supervisor.direct_mode(), 0)

        self.assertEqual(activate.call_count, 2)

    def test_failed_active_verification_clears_the_failed_process(self) -> None:
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
        failed = supervisor.Candidate(0, "vless://failed#failed", "failed")
        fallback = supervisor.Candidate(1, "vless://fallback#fallback", "fallback")
        ranking = [
            supervisor.ProbeResult(failed, True, 10.0, ""),
            supervisor.ProbeResult(fallback, True, 20.0, ""),
        ]

        with mock.patch.object(supervisor, "activate", return_value=True) as activate:
            self.assertTrue(supervisor.failover_from_ranking(ranking, failed.url))

        activate.assert_called_once_with(fallback)

    def test_unavailable_retry_uses_short_cadence_for_refresh_and_probe(self) -> None:
        original_retry = supervisor.UNAVAILABLE_RETRY_SECONDS
        supervisor.UNAVAILABLE_RETRY_SECONDS = 37
        try:
            with mock.patch.object(supervisor.time, "monotonic", return_value=100.0):
                refresh_at, probe_at = supervisor.schedule_unavailable_retry()
        finally:
            supervisor.UNAVAILABLE_RETRY_SECONDS = original_retry

        self.assertEqual(refresh_at, 137.0)
        self.assertEqual(probe_at, 137.0)

    def test_healthcheck_mode_fails_when_active_proxy_probe_fails(self) -> None:
        original_argv = supervisor.sys.argv
        supervisor.sys.argv = ["supervisor.py", "--healthcheck"]
        try:
            with mock.patch.object(supervisor, "verify_active", side_effect=OSError("no upstream")):
                self.assertEqual(supervisor.main(), 1)
        finally:
            supervisor.sys.argv = original_argv


if __name__ == "__main__":
    unittest.main()
