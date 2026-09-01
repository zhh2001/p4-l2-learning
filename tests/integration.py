import collections
import errno
import os
import secrets
import selectors
import signal
import socket
import struct
import sys
import time
import unittest
from dataclasses import dataclass
from pathlib import Path

from scapy.layers.inet import IP, UDP
from scapy.layers.l2 import ARP, Ether
from scapy.packet import Raw


ROOT = Path(__file__).resolve().parents[1]
MININET_DIR = ROOT / "mininet"
sys.path.insert(0, str(MININET_DIR))

from runtime import (  # noqa: E402
    P4RUNTIME_ADDRESS,
    SWITCH_INTERFACES,
    THRIFT_PORT,
    LearningSwitchRuntime,
    process_is_alive,
    split_address,
    tcp_port_open,
)


MAC1 = "00:00:00:00:00:01"
MAC2 = "00:00:00:00:00:02"
MAC3 = "00:00:00:00:00:03"
MAC4 = "00:00:00:00:00:04"
MULTICAST_DESTINATION = "01:00:5e:00:00:01"
INVALID_MULTICAST_SOURCE = "01:00:5e:00:00:09"
ZERO_SOURCE = "00:00:00:00:00:00"
SOL_PACKET = 263
PACKET_ADD_MEMBERSHIP = 1
PACKET_MR_PROMISC = 1


@dataclass(frozen=True)
class EthernetFrame:
    data: bytes
    token: bytes
    source: str
    destination: str
    ether_type: int


def unique_token(label):
    return b"p4-l2:" + label.encode("ascii") + b":" + secrets.token_bytes(12)


def custom_frame(label, source, destination, ether_type=0x88B5):
    token = unique_token(label)
    packet = Ether(src=source, dst=destination, type=ether_type) / Raw(
        token + secrets.token_bytes(64)
    )
    return EthernetFrame(bytes(packet), token, source, destination, ether_type)


def ipv4_frame(label, source, destination):
    token = unique_token(label)
    packet = (
        Ether(src=source, dst=destination, type=0x0800)
        / IP(
            src="192.0.2.2",
            dst="198.51.100.1",
            tos=0xB8,
            ttl=37,
            id=0x4217,
        )
        / UDP(sport=41000, dport=42000)
        / Raw(token + secrets.token_bytes(48))
    )
    return EthernetFrame(bytes(packet), token, source, destination, 0x0800)


def arp_frame(label, source, destination):
    token = unique_token(label)
    packet = (
        Ether(src=source, dst=destination, type=0x0806)
        / ARP(
            op=1,
            hwsrc=source,
            psrc="10.0.0.3",
            hwdst="00:00:00:00:00:00",
            pdst="10.0.0.99",
        )
        / Raw(token + secrets.token_bytes(32))
    )
    return EthernetFrame(bytes(packet), token, source, destination, 0x0806)


class PortCapture:
    def __init__(self, runtime):
        self.runtime = runtime
        self.selector = selectors.DefaultSelector()
        self.sockets = {}
        self.frames = {}
        self.counts = {}
        self.corrupted = {}
        self.packet_types = {}
        self.expected = {}
        self.context = {}

    def __enter__(self):
        protocol = socket.htons(socket.ETH_P_ALL)
        root_namespace = os.open("/proc/self/ns/net", os.O_RDONLY)
        try:
            for port in range(1, 5):
                host = self.runtime.hosts[f"h{port}"]
                target_namespace = os.open(
                    f"/proc/{host.shell.pid}/ns/net", os.O_RDONLY
                )
                try:
                    os.setns(target_namespace, os.CLONE_NEWNET)
                    interface = host.defaultIntf().name
                    capture = socket.socket(
                        socket.AF_PACKET,
                        socket.SOCK_RAW,
                        protocol,
                    )
                    try:
                        capture.setblocking(False)
                        capture.bind((interface, 0))
                        membership = struct.pack(
                            "IHH8s",
                            socket.if_nametoindex(interface),
                            PACKET_MR_PROMISC,
                            0,
                            b"",
                        )
                        capture.setsockopt(
                            SOL_PACKET,
                            PACKET_ADD_MEMBERSHIP,
                            membership,
                        )
                        self.selector.register(
                            capture,
                            selectors.EVENT_READ,
                            port,
                        )
                    except BaseException:
                        capture.close()
                        raise
                    self.sockets[port] = capture
                finally:
                    os.setns(root_namespace, os.CLONE_NEWNET)
                    os.close(target_namespace)
        except BaseException as error:
            self.__exit__(type(error), error, error.__traceback__)
            raise
        finally:
            os.close(root_namespace)
        return self

    def __exit__(self, _exc_type, _exc_value, _traceback):
        errors = []
        for capture in self.sockets.values():
            try:
                self.selector.unregister(capture)
            except Exception as error:
                errors.append(f"unregister capture: {error}")
            try:
                capture.close()
            except OSError as error:
                errors.append(f"close capture: {error}")
        try:
            self.selector.close()
        except OSError as error:
            errors.append(f"close selector: {error}")
        self.sockets.clear()
        if errors:
            detail = "; ".join(errors)
            if _exc_value is not None:
                raise RuntimeError(
                    f"{_exc_value}; capture cleanup failed: {detail}"
                ) from _exc_value
            raise RuntimeError(detail)

    def register(self, frame, expected_ports, context):
        self.frames[frame.token] = frame
        self.counts[frame.token] = collections.Counter()
        self.corrupted[frame.token] = []
        self.packet_types[frame.token] = collections.Counter()
        self.expected[frame.token] = collections.Counter(expected_ports)
        self.context[frame.token] = context

    def _record(self, port, packet, packet_type):
        for token, frame in self.frames.items():
            if token not in packet:
                continue
            self.packet_types[token][(port, packet_type)] += 1
            if packet_type == socket.PACKET_OUTGOING:
                return
            self.counts[token][port] += 1
            if packet != frame.data:
                self.corrupted[token].append((port, packet))
            return

    def collect(self, duration):
        deadline = time.monotonic() + duration
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return
            events = self.selector.select(remaining)
            for key, _mask in events:
                while True:
                    try:
                        packet, address = key.fileobj.recvfrom(65535)
                    except BlockingIOError:
                        break
                    except OSError as error:
                        if error.errno == errno.ENETDOWN:
                            break
                        raise
                    self._record(key.data, packet, address[2])

    def drain(self):
        while True:
            events = self.selector.select(0)
            if not events:
                return
            for key, _mask in events:
                while True:
                    try:
                        packet, address = key.fileobj.recvfrom(65535)
                    except BlockingIOError:
                        break
                    except OSError as error:
                        if error.errno == errno.ENETDOWN:
                            break
                        raise
                    self._record(key.data, packet, address[2])

    def assert_exchange(
        self,
        test,
        runtime,
        host_name,
        frame,
        expected_ports,
        label,
        observation=0.25,
    ):
        context = (
            f"{label}: token={frame.token.hex()} src={frame.source} "
            f"dst={frame.destination} ingress={host_name} "
            f"ethertype=0x{frame.ether_type:04x}"
        )
        self.drain()
        self.assert_all(test)
        self.register(frame, expected_ports, context)
        send_frame(runtime.hosts[host_name], frame.data)
        self.collect(observation)
        self.assert_all(test)

    def assert_all(self, test):
        for token, expected in self.expected.items():
            observed = self.counts[token]
            detail = (
                f"{self.context[token]} expected={dict(expected)} "
                f"observed={dict(observed)} "
                f"packet_types={dict(self.packet_types[token])}"
            )
            test.assertEqual(observed, expected, detail)
            test.assertEqual(self.corrupted[token], [], detail)

    def assert_stable(self, test, duration=0.2):
        self.collect(duration)
        self.assert_all(test)


def send_frame(host, frame):
    interface = host.defaultIntf().name
    root_namespace = os.open("/proc/self/ns/net", os.O_RDONLY)
    target_namespace = None
    try:
        target_namespace = os.open(
            f"/proc/{host.shell.pid}/ns/net",
            os.O_RDONLY,
        )
        os.setns(target_namespace, os.CLONE_NEWNET)
        with socket.socket(socket.AF_PACKET, socket.SOCK_RAW) as sender:
            sender.bind((interface, 0))
            sent = sender.send(frame)
    finally:
        os.setns(root_namespace, os.CLONE_NEWNET)
        if target_namespace is not None:
            os.close(target_namespace)
        os.close(root_namespace)
    if sent != len(frame):
        raise AssertionError(
            f"packet sender on {host.name} sent {sent} of {len(frame)} bytes"
        )


def wait_for_learned(test, runtime, mac, port, timeout=4.0):
    deadline = time.monotonic() + timeout
    last_detail = "no readback attempted"
    while True:
        runtime.ensure_controller_alive()
        result = runtime.query(
            "--expect-mac",
            mac,
            "--expect-port",
            str(port),
        )
        if result.returncode == 0:
            return
        last_detail = (result.stderr or result.stdout).strip()
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            test.fail(
                f"learned-state timeout: expected {mac} -> port {port}; "
                f"last readback: {last_detail}"
            )
        time.sleep(min(0.05, remaining))


def assert_absent_for(test, runtime, mac, duration=0.6):
    deadline = time.monotonic() + duration
    attempts = 0
    while True:
        runtime.ensure_controller_alive()
        result = runtime.query("--expect-absent-mac", mac)
        attempts += 1
        if result.returncode != 0:
            detail = (result.stderr or result.stdout).strip()
            test.fail(f"unexpected learned state for {mac}: {detail}")
        remaining = deadline - time.monotonic()
        if remaining <= 0 and attempts >= 2:
            return
        time.sleep(min(0.05, max(remaining, 0.0)))


class RuntimeIntegrationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if os.geteuid() != 0:
            raise unittest.SkipTest("runtime integration requires root privileges")

    def test_failure_path_cleans_owned_resources(self):
        runtime = LearningSwitchRuntime(ROOT)
        self.addCleanup(runtime.stop)
        runtime.device_config = runtime.runtime_dir / "missing.json"
        runtime_dir = runtime.runtime_dir

        with self.assertRaisesRegex(RuntimeError, "read device config"):
            runtime.start()

        self.assertTrue(runtime.cleanup_complete)
        self.assertFalse(runtime_dir.exists())
        for name, process, marker in runtime.processes:
            self.assertIsNotNone(process.poll(), f"{name} process was not reaped")
            self.assertFalse(process_is_alive(marker), f"{name} process remains")
        for name, marker in runtime.node_processes:
            self.assertFalse(process_is_alive(marker), f"{name} shell remains")
        grpc_host, grpc_port = split_address(P4RUNTIME_ADDRESS)
        self.assertFalse(tcp_port_open(grpc_host, grpc_port))
        self.assertFalse(tcp_port_open("127.0.0.1", THRIFT_PORT))
        for interface in SWITCH_INTERFACES:
            with self.assertRaises(OSError):
                socket.if_nametoindex(interface)

    def test_learning_flooding_moves_and_transparency(self):
        runtime = LearningSwitchRuntime(ROOT)
        runtime_dir = runtime.runtime_dir
        try:
            runtime.start(enable_links=False)
            runtime.enable_links()
            with PortCapture(runtime) as capture:
                initial = custom_frame("initial-unknown", MAC1, MAC2)
                capture.assert_exchange(
                    self, runtime, "h1", initial, [2, 3, 4], "initial unknown unicast"
                )
                wait_for_learned(self, runtime, MAC1, 1)

                reverse = ipv4_frame("reverse-known", MAC2, MAC1)
                capture.assert_exchange(
                    self, runtime, "h2", reverse, [1], "reverse known unicast"
                )
                wait_for_learned(self, runtime, MAC2, 2)

                second = custom_frame("second-known", MAC1, MAC2, 0x88B6)
                capture.assert_exchange(
                    self, runtime, "h1", second, [2], "second forward known unicast"
                )

                broadcast = arp_frame(
                    "broadcast", MAC3, "ff:ff:ff:ff:ff:ff"
                )
                capture.assert_exchange(
                    self, runtime, "h3", broadcast, [1, 2, 4], "broadcast flood"
                )
                wait_for_learned(self, runtime, MAC3, 3)
                assert_absent_for(self, runtime, "ff:ff:ff:ff:ff:ff", 0.2)

                multicast = custom_frame(
                    "ethernet-multicast", MAC4, MULTICAST_DESTINATION, 0x86DD
                )
                capture.assert_exchange(
                    self,
                    runtime,
                    "h4",
                    multicast,
                    [1, 2, 3],
                    "Ethernet multicast flood",
                )
                wait_for_learned(self, runtime, MAC4, 4)
                assert_absent_for(self, runtime, MULTICAST_DESTINATION, 0.2)

                same_port = custom_frame("same-port", MAC3, MAC3)
                capture.assert_exchange(
                    self, runtime, "h3", same_port, [], "same-port known destination"
                )

                for sequence in range(3):
                    duplicate = custom_frame(
                        f"duplicate-{sequence}", MAC1, MAC2, 0x9000 + sequence
                    )
                    capture.assert_exchange(
                        self,
                        runtime,
                        "h1",
                        duplicate,
                        [2],
                        f"stable known unicast {sequence}",
                    )
                wait_for_learned(self, runtime, MAC1, 1)

                moved = custom_frame(
                    "mac-move", MAC1, "00:00:00:00:00:99", 0x88B7
                )
                capture.assert_exchange(
                    self, runtime, "h3", moved, [1, 2, 4], "MAC move trigger"
                )
                wait_for_learned(self, runtime, MAC1, 3)

                moved_destination = custom_frame(
                    "moved-destination", MAC2, MAC1, 0x88B8
                )
                capture.assert_exchange(
                    self,
                    runtime,
                    "h2",
                    moved_destination,
                    [3],
                    "unicast after MAC move",
                )

                stable_move = custom_frame("stable-move", MAC1, MAC2, 0x88B9)
                capture.assert_exchange(
                    self,
                    runtime,
                    "h3",
                    stable_move,
                    [2],
                    "stable forwarding after MAC move",
                )
                wait_for_learned(self, runtime, MAC1, 3)

                invalid_source = custom_frame(
                    "invalid-multicast-source",
                    INVALID_MULTICAST_SOURCE,
                    MAC2,
                    0x88BA,
                )
                capture.assert_exchange(
                    self,
                    runtime,
                    "h4",
                    invalid_source,
                    [],
                    "invalid multicast source",
                )
                assert_absent_for(self, runtime, INVALID_MULTICAST_SOURCE)

                zero_source = custom_frame(
                    "invalid-zero-source", ZERO_SOURCE, MAC2, 0x88BB
                )
                capture.assert_exchange(
                    self, runtime, "h4", zero_source, [], "invalid zero source"
                )
                assert_absent_for(self, runtime, ZERO_SOURCE)

                for mac, port in ((MAC1, 3), (MAC2, 2), (MAC3, 3), (MAC4, 4)):
                    wait_for_learned(self, runtime, mac, port)
                capture.assert_stable(self)
                runtime.ensure_controller_alive()
        finally:
            runtime.stop()

        self.assertTrue(runtime.cleanup_complete)
        self.assertFalse(runtime_dir.exists())


def main():
    interrupted = False

    def terminate(_signal_number, _frame):
        nonlocal interrupted
        if interrupted:
            return
        interrupted = True
        raise KeyboardInterrupt

    handled = (signal.SIGTERM, signal.SIGHUP)
    previous = {
        signal_number: signal.signal(signal_number, terminate)
        for signal_number in handled
    }
    try:
        unittest.main(verbosity=2)
    finally:
        for signal_number, handler in previous.items():
            signal.signal(signal_number, handler)


if __name__ == "__main__":
    main()
