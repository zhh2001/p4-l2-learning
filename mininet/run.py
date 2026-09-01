import os
import signal
import sys
from pathlib import Path

from mininet.cli import CLI

sys.path.insert(0, str(Path(__file__).resolve().parent))

from runtime import LearningSwitchRuntime  # noqa: E402


ROOT = Path(__file__).resolve().parents[1]


def main():
    if os.geteuid() != 0:
        raise SystemExit("Mininet runtime requires root privileges")

    def terminate(_signal_number, _frame):
        raise SystemExit(0)

    handled = (signal.SIGTERM, signal.SIGHUP)
    previous_sigint = signal.getsignal(signal.SIGINT)
    previous = {
        signal_number: signal.signal(signal_number, terminate)
        for signal_number in handled
    }
    runtime = LearningSwitchRuntime(ROOT)
    try:
        try:
            runtime.start()
            CLI(runtime.network)
        except KeyboardInterrupt:
            pass
    finally:
        cleanup_signals = (*handled, signal.SIGINT)
        for signal_number in cleanup_signals:
            signal.signal(signal_number, signal.SIG_IGN)
        try:
            runtime.stop()
        finally:
            for signal_number, handler in previous.items():
                signal.signal(signal_number, handler)
            signal.signal(signal.SIGINT, previous_sigint)


if __name__ == "__main__":
    main()
