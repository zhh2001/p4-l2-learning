import socket
import subprocess
import time
from functools import partial
from pathlib import Path

from mininet.net import Mininet
from mininet.node import Host, Switch
from mininet.topo import Topo


P4RUNTIME_ADDRESS = "127.0.0.1:50052"
THRIFT_PORT = 9091
DEVICE_ID = 2
SWITCH_PORTS = {f"h{port}": port for port in range(1, 5)}
HOSTS = {
    f"h{host}": {
        "ip": f"10.0.0.{host}/24",
        "mac": f"00:00:00:00:00:{host:02x}",
    }
    for host in range(1, 5)
}


def split_address(address):
    host, separator, port = address.rpartition(":")
    if not separator or not host or not port.isdecimal():
        raise ValueError(f"invalid TCP address: {address}")
    return host, int(port)


def tcp_port_open(host, port):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as connection:
        connection.settimeout(0.05)
        return connection.connect_ex((host, port)) == 0


def bmv2_command(
    interfaces,
    binary="simple_switch_grpc",
    grpc_address=P4RUNTIME_ADDRESS,
    thrift_port=THRIFT_PORT,
    device_id=DEVICE_ID,
):
    expected_interfaces = {
        port: f"s1-eth{port}" for port in SWITCH_PORTS.values()
    }
    if interfaces != expected_interfaces:
        raise ValueError(f"BMv2 interfaces must be {expected_interfaces}")

    command = [binary]
    for port in sorted(interfaces):
        command.extend(["-i", f"{port}@{interfaces[port]}"])
    command.extend(
        [
            "--device-id",
            str(device_id),
            "--thrift-port",
            str(thrift_port),
            "--log-console",
            "--log-level",
            "info",
            "--no-p4",
            "--",
            "--grpc-server-addr",
            grpc_address,
        ]
    )
    return command


class QuietHost(Host):
    def config(self, **params):
        result = super().config(**params)
        self.cmd("ethtool -K", self.defaultIntf(), "rx off tx off sg off")
        for setting in ("all", "default", "lo"):
            self.cmd(
                "sysctl -q -w",
                f"net.ipv6.conf.{setting}.disable_ipv6=1",
            )
        return result


class P4RuntimeSwitch(Switch):
    def __init__(
        self,
        name,
        runtime_dir,
        binary="simple_switch_grpc",
        grpc_address=P4RUNTIME_ADDRESS,
        thrift_port=THRIFT_PORT,
        device_id=DEVICE_ID,
        startup_timeout=5.0,
        **params,
    ):
        if params.get("inNamespace"):
            raise ValueError("the BMv2 switch must run in the root network namespace")
        runtime_dir = Path(runtime_dir)
        if not runtime_dir.is_absolute():
            raise ValueError("runtime directory must be absolute")
        super().__init__(name, **params)
        self.runtime_dir = runtime_dir
        self.binary = binary
        self.grpc_address = grpc_address
        self.thrift_port = thrift_port
        self.device_id = device_id
        self.startup_timeout = startup_timeout
        self.process = None
        self.log_file = None

    def _interfaces(self):
        return {
            port: interface.name
            for port, interface in self.intfs.items()
            if port > 0 and interface.link is not None
        }

    def _wait_ready(self):
        grpc_host, grpc_port = split_address(self.grpc_address)
        endpoints = ((grpc_host, grpc_port), ("127.0.0.1", self.thrift_port))
        deadline = time.monotonic() + self.startup_timeout
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                return False
            if all(tcp_port_open(host, port) for host, port in endpoints):
                return True
            time.sleep(0.05)
        return False

    def _stop_process(self):
        process = self.process
        self.process = None
        try:
            if process is not None and process.poll() is None:
                try:
                    process.terminate()
                except ProcessLookupError:
                    pass
                try:
                    process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    try:
                        process.kill()
                    except ProcessLookupError:
                        pass
                    process.wait(timeout=2)
        finally:
            if self.log_file is not None:
                self.log_file.close()
                self.log_file = None

    def start(self, _controllers):
        if self.process is not None:
            raise RuntimeError(f"{self.name} is already running")

        grpc_host, grpc_port = split_address(self.grpc_address)
        for host, port in ((grpc_host, grpc_port), ("127.0.0.1", self.thrift_port)):
            if tcp_port_open(host, port):
                raise RuntimeError(f"TCP port {host}:{port} is already in use")

        log_path = self.runtime_dir / "bmv2.log"
        command = bmv2_command(
            self._interfaces(),
            binary=self.binary,
            grpc_address=self.grpc_address,
            thrift_port=self.thrift_port,
            device_id=self.device_id,
        )
        self.runtime_dir.mkdir(parents=True, exist_ok=True)
        self.log_file = log_path.open("w", encoding="utf-8")
        try:
            self.process = subprocess.Popen(
                command,
                stdout=self.log_file,
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )
        except OSError:
            self._stop_process()
            raise

        if not self._wait_ready():
            return_code = self.process.poll()
            self._stop_process()
            failure = (
                "readiness timeout"
                if return_code is None
                else f"exit status {return_code}"
            )
            raise RuntimeError(
                f"{self.name} failed to start; {failure}, log {log_path}"
            )

    def stop(self, deleteIntfs=True):
        self._stop_process()
        super().stop(deleteIntfs)

    def terminate(self):
        self._stop_process()
        super().terminate()


class LearningSwitchTopo(Topo):
    def build(self):
        switch = self.addSwitch("s1")
        for host, address in HOSTS.items():
            self.addHost(host, **address)
            self.addLink(host, switch, port1=0, port2=SWITCH_PORTS[host])


def create_network(runtime_dir, switch_binary="simple_switch_grpc"):
    switch = partial(
        P4RuntimeSwitch,
        runtime_dir=runtime_dir,
        binary=switch_binary,
    )
    return Mininet(
        topo=LearningSwitchTopo(),
        host=QuietHost,
        switch=switch,
        controller=None,
        autoSetMacs=False,
        autoStaticArp=False,
    )
