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


if __name__ == "__main__":
    unittest.main()
