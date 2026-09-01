import os
import socket
import subprocess
import tempfile
import time
from pathlib import Path

from topology import (
    HOSTS,
    P4RUNTIME_ADDRESS,
    SWITCH_PORTS,
    THRIFT_PORT,
    create_network,
    split_address,
    tcp_port_open,
)


SWITCH_INTERFACES = tuple(f"s1-eth{port}" for port in SWITCH_PORTS.values())


def process_marker(pid):
    try:
        stat = Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
    except (FileNotFoundError, ProcessLookupError):
        return None
    fields = stat.rsplit(")", 1)[1].split()
    return pid, fields[19]


def process_is_alive(marker):
    if marker is None:
        return False
    return process_marker(marker[0]) == marker


def wait_until(predicate, timeout, interval=0.05):
    deadline = time.monotonic() + timeout
    while True:
        if predicate():
            return True
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return False
        time.sleep(min(interval, remaining))


class LearningSwitchRuntime:
    def __init__(
        self,
        project_root,
        controller_binary=None,
        p4info=None,
        device_config=None,
        switch_binary="simple_switch_grpc",
        startup_timeout=10.0,
    ):
        self.project_root = Path(project_root).resolve()
        self.controller_binary = Path(
            controller_binary
            or self.project_root / "build" / "learning-controller"
        )
        self.p4info = Path(
            p4info
            or self.project_root / "build" / "learning_switch.p4info.txtpb"
        )
        self.device_config = Path(
            device_config
            or self.project_root / "build" / "learning_switch.json"
        )
        self.switch_binary = switch_binary
        self.startup_timeout = startup_timeout
        self._temporary = tempfile.TemporaryDirectory(prefix="p4-l2-learning-")
        self.runtime_dir = Path(self._temporary.name)
        self.network = None
        self.controller = None
        self.controller_was_ready = False
        self.controller_log = None
        self.processes = []
        self.node_processes = []
        self.resources_created = False
        self.links_enabled = False
        self.cleanup_complete = False
        self.last_log_tail = ""

    def __enter__(self):
        return self.start()

    def __exit__(self, _exc_type, _exc_value, _traceback):
        try:
            self.stop()
        except Exception as cleanup_error:
            if _exc_value is not None:
                raise RuntimeError(
                    f"{_exc_value}; runtime cleanup failed: {cleanup_error}"
                ) from _exc_value
            raise

    def _preflight(self):
        if os.geteuid() != 0:
            raise PermissionError("Mininet runtime requires root privileges")
        if not self.controller_binary.is_file():
            raise FileNotFoundError(
                f"controller binary not found: {self.controller_binary}"
            )
        existing = []
        for interface in SWITCH_INTERFACES:
            try:
                socket.if_nametoindex(interface)
            except OSError:
                continue
            existing.append(interface)
        if existing:
            raise RuntimeError(f"switch interfaces already exist: {existing}")

        grpc_host, grpc_port = split_address(P4RUNTIME_ADDRESS)
        occupied = [
            f"{host}:{port}"
            for host, port in (
                (grpc_host, grpc_port),
                ("127.0.0.1", THRIFT_PORT),
            )
            if tcp_port_open(host, port)
        ]
        if occupied:
            raise RuntimeError(f"control ports already in use: {occupied}")

    def _set_links(self, status):
        for host in SWITCH_PORTS:
            self.network[host].defaultIntf().ifconfig(status)
        self.links_enabled = status == "up"

    def _controller_command(self, *extra):
        return [
            str(self.controller_binary),
            "--p4info",
            str(self.p4info),
            "--device-config",
            str(self.device_config),
            *extra,
        ]

    def _start_controller(self):
        log_path = self.runtime_dir / "controller.log"
        self.controller_log = log_path.open("w", encoding="utf-8")
        self.controller = subprocess.Popen(
            self._controller_command(),
            cwd=self.project_root,
            stdout=self.controller_log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        self.processes.append(
            ("controller", self.controller, process_marker(self.controller.pid))
        )

    def _log_tail(self, lines=20):
        path = self.runtime_dir / "controller.log"
        try:
            content = path.read_text(encoding="utf-8", errors="replace").splitlines()
        except FileNotFoundError:
            return ""
        return "\n".join(content[-lines:])

    def _wait_controller_ready(self):
        def ready():
            if self.controller.poll() is not None:
                return True
            return "controller ready" in self._log_tail()

        if not wait_until(ready, self.startup_timeout):
            raise RuntimeError("controller readiness timeout")
        if self.controller.poll() is not None:
            raise RuntimeError(
                f"controller exited with status {self.controller.returncode}"
            )
        if "controller ready" not in self._log_tail():
            raise RuntimeError("controller exited before readiness")
        self.controller_was_ready = True

    def query(self, *arguments, timeout=5.0):
        return subprocess.run(
            self._controller_command(*arguments),
            cwd=self.project_root,
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout,
        )

    def ensure_controller_alive(self):
        if self.controller is None or self.controller.poll() is not None:
            status = None if self.controller is None else self.controller.returncode
            raise RuntimeError(f"controller is not running; status {status}")

    def enable_links(self):
        self.ensure_controller_alive()
        if not self.links_enabled:
            self._set_links("up")

    def start(self, enable_links=True):
        if self.cleanup_complete or self._temporary is None:
            raise RuntimeError("runtime is closed")
        if self.resources_created:
            raise RuntimeError("runtime is already started")
        try:
            self._preflight()
            self.network = create_network(
                self.runtime_dir,
                switch_binary=self.switch_binary,
                build=False,
            )
            self.resources_created = True
            self.network.build()
            for node in [*self.network.hosts, *self.network.switches]:
                shell = getattr(node, "shell", None)
                if shell is not None:
                    self.node_processes.append(
                        (node.name, process_marker(shell.pid))
                    )
            # Gate host transmission without lowering BMv2-facing interfaces.
            self._set_links("down")
            self.network.start()
            switch_process = self.network["s1"].process
            self.processes.append(
                ("BMv2", switch_process, process_marker(switch_process.pid))
            )
            self._start_controller()
            self._wait_controller_ready()
            verification = self.query("--verify-only")
            if verification.returncode != 0:
                detail = (verification.stderr or verification.stdout).strip()
                raise RuntimeError(f"startup state verification failed: {detail}")
            if enable_links:
                self.enable_links()
            return self
        except BaseException as error:
            self.last_log_tail = self._log_tail()
            try:
                self.stop()
            except Exception as cleanup_error:
                raise RuntimeError(
                    f"runtime startup failed: {error}; cleanup failed: {cleanup_error}"
                ) from error
            if not isinstance(error, Exception):
                raise
            detail = f"\n{self.last_log_tail}" if self.last_log_tail else ""
            raise RuntimeError(f"runtime startup failed: {error}{detail}") from error

    def _stop_controller(self):
        if self.controller is None:
            return
        was_running = self.controller.poll() is None
        if was_running:
            try:
                self.controller.terminate()
            except ProcessLookupError:
                pass
            try:
                self.controller.wait(timeout=10)
            except subprocess.TimeoutExpired:
                try:
                    self.controller.kill()
                except ProcessLookupError:
                    pass
                self.controller.wait(timeout=2)
        else:
            self.controller.wait()
        if self.controller_was_ready and not was_running:
            detail = self._log_tail()
            message = (
                "controller exited unexpectedly with status "
                f"{self.controller.returncode}"
            )
            if detail:
                message += f"; log tail:\n{detail}"
            raise RuntimeError(message)
        if was_running and self.controller.returncode != 0:
            raise RuntimeError(
                f"controller stopped with status {self.controller.returncode}"
            )

    def _fallback_stop_nodes(self):
        if self.network is None:
            return
        errors = []
        for switch in self.network.switches:
            try:
                switch.stop()
            except Exception as error:
                errors.append(f"stop {switch.name}: {error}")
            try:
                switch.terminate()
            except Exception as error:
                errors.append(f"terminate {switch.name}: {error}")
        for host in self.network.hosts:
            try:
                host.terminate()
            except Exception as error:
                errors.append(f"terminate {host.name}: {error}")
        if errors:
            raise RuntimeError("; ".join(errors))

    def _check_cleanup(self):
        errors = []
        if self.processes:
            if not wait_until(
                lambda: all(
                    not process_is_alive(marker)
                    for _name, _process, marker in self.processes
                ),
                2.0,
            ):
                alive = [
                    name
                    for name, _process, marker in self.processes
                    if process_is_alive(marker)
                ]
                errors.append(f"owned processes remain: {alive}")
        if self.node_processes:
            if not wait_until(
                lambda: all(
                    not process_is_alive(marker)
                    for _name, marker in self.node_processes
                ),
                2.0,
            ):
                alive = [
                    name
                    for name, marker in self.node_processes
                    if process_is_alive(marker)
                ]
                errors.append(f"Mininet node processes remain: {alive}")

        grpc_host, grpc_port = split_address(P4RUNTIME_ADDRESS)
        endpoints = ((grpc_host, grpc_port), ("127.0.0.1", THRIFT_PORT))
        if self.processes and not wait_until(
            lambda: all(not tcp_port_open(host, port) for host, port in endpoints),
            2.0,
        ):
            errors.append("control ports remain open")

        existing = []
        for interface in SWITCH_INTERFACES:
            try:
                socket.if_nametoindex(interface)
            except OSError:
                continue
            existing.append(interface)
        if self.resources_created and existing:
            errors.append(f"switch interfaces remain: {existing}")
        return errors

    def stop(self):
        if self.cleanup_complete:
            return
        errors = []
        try:
            self._stop_controller()
        except Exception as error:
            errors.append(f"stop controller: {error}")

        if self.network is not None:
            try:
                self.network.stop()
            except Exception as error:
                errors.append(f"stop Mininet: {error}")
                try:
                    self._fallback_stop_nodes()
                except Exception as fallback_error:
                    errors.append(f"fallback Mininet cleanup: {fallback_error}")

        if self.controller_log is not None:
            try:
                self.controller_log.close()
            except OSError as error:
                errors.append(f"close controller log: {error}")
            self.controller_log = None

        errors.extend(self._check_cleanup())
        if self._temporary is not None:
            try:
                self._temporary.cleanup()
            except OSError as error:
                errors.append(f"remove runtime directory: {error}")
            self._temporary = None

        self.cleanup_complete = not errors
        if errors:
            raise RuntimeError("; ".join(errors))

    @property
    def hosts(self):
        if self.network is None:
            return {}
        return {name: self.network[name] for name in HOSTS}
