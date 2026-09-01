import importlib.util
import subprocess
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "mininet" / "topology.py"
SPEC = importlib.util.spec_from_file_location("learning_switch_topology", MODULE_PATH)
TOPOLOGY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TOPOLOGY)


class TopologyTest(unittest.TestCase):
    def setUp(self):
        self.topology = TOPOLOGY.LearningSwitchTopo()

    def test_has_one_switch_and_four_deterministic_hosts(self):
        self.assertEqual(self.topology.switches(), ["s1"])
        self.assertEqual(self.topology.hosts(), ["h1", "h2", "h3", "h4"])
        self.assertEqual(
            {host: self.topology.nodeInfo(host) for host in self.topology.hosts()},
            TOPOLOGY.HOSTS,
        )

    def test_host_links_use_switch_ports_one_through_four(self):
        self.assertEqual(len(self.topology.links()), 4)
        for host, switch_port in TOPOLOGY.SWITCH_PORTS.items():
            self.assertEqual(self.topology.port(host, "s1"), (0, switch_port))

    def test_runtime_identifiers_are_deterministic(self):
        self.assertEqual(TOPOLOGY.P4RUNTIME_ADDRESS, "127.0.0.1:50052")
        self.assertEqual(TOPOLOGY.THRIFT_PORT, 9091)
        self.assertEqual(TOPOLOGY.DEVICE_ID, 2)

    def test_network_disables_automatic_mac_and_arp_configuration(self):
        runtime_dir = ROOT / "runtime"
        with mock.patch.object(TOPOLOGY, "Mininet") as mininet:
            TOPOLOGY.create_network(runtime_dir)

        options = mininet.call_args.kwargs
        self.assertIs(options["host"], TOPOLOGY.QuietHost)
        self.assertIsNone(options["controller"])
        self.assertFalse(options["autoSetMacs"])
        self.assertFalse(options["autoStaticArp"])
        self.assertIsInstance(options["topo"], TOPOLOGY.LearningSwitchTopo)
        self.assertIs(options["switch"].func, TOPOLOGY.P4RuntimeSwitch)
        self.assertEqual(options["switch"].keywords["runtime_dir"], runtime_dir)


class BMv2CommandTest(unittest.TestCase):
    def test_command_binds_exact_data_and_control_ports(self):
        interfaces = {port: f"s1-eth{port}" for port in range(1, 5)}
        command = TOPOLOGY.bmv2_command(interfaces)
        self.assertEqual(
            command,
            [
                "simple_switch_grpc",
                "-i",
                "1@s1-eth1",
                "-i",
                "2@s1-eth2",
                "-i",
                "3@s1-eth3",
                "-i",
                "4@s1-eth4",
                "--device-id",
                "2",
                "--thrift-port",
                "9091",
                "--log-console",
                "--log-level",
                "info",
                "--no-p4",
                "--",
                "--grpc-server-addr",
                "127.0.0.1:50052",
            ],
        )

    def test_command_rejects_an_incomplete_port_set(self):
        with self.assertRaisesRegex(ValueError, "interfaces"):
            TOPOLOGY.bmv2_command(
                {1: "s1-eth1", 2: "s1-eth2", 3: "s1-eth3"},
            )

    def test_command_rejects_a_mismatched_interface(self):
        interfaces = {port: f"s1-eth{port}" for port in range(1, 5)}
        interfaces[4] = "s1-eth3"
        with self.assertRaisesRegex(ValueError, "interfaces"):
            TOPOLOGY.bmv2_command(interfaces)

    def test_switch_rejects_a_relative_runtime_directory(self):
        with self.assertRaisesRegex(ValueError, "absolute"):
            TOPOLOGY.P4RuntimeSwitch("s1", Path("runtime"))


class BMv2LifecycleTest(unittest.TestCase):
    def test_stop_terminates_only_the_owned_process(self):
        process = mock.Mock()
        process.poll.return_value = None
        switch = mock.Mock(
            process=process,
            log_file=None,
            spec=TOPOLOGY.P4RuntimeSwitch,
        )

        TOPOLOGY.P4RuntimeSwitch._stop_process(switch)

        process.terminate.assert_called_once_with()
        process.kill.assert_not_called()
        process.wait.assert_called_once_with(timeout=2)
        self.assertIsNone(switch.process)

    def test_stop_escalates_when_graceful_shutdown_times_out(self):
        process = mock.Mock()
        process.poll.return_value = None
        process.wait.side_effect = [subprocess.TimeoutExpired("bmv2", 2), None]
        switch = mock.Mock(
            process=process,
            log_file=None,
            spec=TOPOLOGY.P4RuntimeSwitch,
        )

        TOPOLOGY.P4RuntimeSwitch._stop_process(switch)

        process.terminate.assert_called_once_with()
        process.kill.assert_called_once_with()
        self.assertEqual(process.wait.call_count, 2)
        self.assertIsNone(switch.process)

    def test_stop_closes_the_log_if_the_process_disappears(self):
        process = mock.Mock()
        process.poll.return_value = None
        process.terminate.side_effect = ProcessLookupError
        log_file = mock.Mock()
        switch = mock.Mock(
            process=process,
            log_file=log_file,
            spec=TOPOLOGY.P4RuntimeSwitch,
        )

        TOPOLOGY.P4RuntimeSwitch._stop_process(switch)

        log_file.close.assert_called_once_with()
        self.assertIsNone(switch.log_file)


if __name__ == "__main__":
    unittest.main()
