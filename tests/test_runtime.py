import sys
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "mininet"))

import runtime as RUNTIME  # noqa: E402


class RuntimeLifecycleTest(unittest.TestCase):
    def test_runtime_is_single_use_after_cleanup(self):
        runtime = RUNTIME.LearningSwitchRuntime(ROOT)
        runtime_dir = runtime.runtime_dir

        runtime.stop()

        self.assertTrue(runtime.cleanup_complete)
        self.assertFalse(runtime_dir.exists())
        with self.assertRaisesRegex(RuntimeError, "closed"):
            runtime.start()
        runtime.stop()

    def test_startup_interrupt_cleans_a_partially_built_network(self):
        network = mock.Mock()
        network.build.side_effect = KeyboardInterrupt
        runtime = RUNTIME.LearningSwitchRuntime(ROOT)
        runtime_dir = runtime.runtime_dir

        with mock.patch.object(runtime, "_preflight"), mock.patch.object(
            runtime,
            "_check_cleanup",
            return_value=[],
        ), mock.patch.object(RUNTIME, "create_network", return_value=network):
            with self.assertRaises(KeyboardInterrupt):
                runtime.start()

        network.stop.assert_called_once_with()
        self.assertTrue(runtime.cleanup_complete)
        self.assertFalse(runtime_dir.exists())

    def test_stop_reports_a_controller_that_exited_after_readiness(self):
        runtime = RUNTIME.LearningSwitchRuntime(ROOT)
        runtime_dir = runtime.runtime_dir
        (runtime_dir / "controller.log").write_text(
            "controller ready\ndigest stream failed\n",
            encoding="utf-8",
        )
        process = mock.Mock(returncode=17)
        process.poll.return_value = 17
        runtime.controller = process
        runtime.controller_was_ready = True

        with self.assertRaisesRegex(
            RuntimeError,
            "controller exited unexpectedly with status 17",
        ) as raised:
            runtime.stop()

        self.assertIn("digest stream failed", str(raised.exception))
        process.wait.assert_called_once_with()
        self.assertFalse(runtime_dir.exists())

    def test_controller_readiness_is_recorded(self):
        runtime = RUNTIME.LearningSwitchRuntime(ROOT)
        process = mock.Mock()
        process.poll.return_value = None
        runtime.controller = process
        try:
            with mock.patch.object(
                runtime,
                "_log_tail",
                return_value="controller ready",
            ):
                runtime._wait_controller_ready()

            self.assertTrue(runtime.controller_was_ready)
        finally:
            runtime.controller = None
            runtime.stop()

    def test_stop_accepts_an_exit_before_readiness(self):
        runtime = RUNTIME.LearningSwitchRuntime(ROOT)
        runtime_dir = runtime.runtime_dir
        process = mock.Mock(returncode=1)
        process.poll.return_value = 1
        runtime.controller = process

        runtime.stop()

        process.wait.assert_called_once_with()
        self.assertTrue(runtime.cleanup_complete)
        self.assertFalse(runtime_dir.exists())


if __name__ == "__main__":
    unittest.main()
