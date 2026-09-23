"""pytest fixtures for the lux end-to-end suite.

Tests drive lux through its CLI wherever possible, so every test exercises
the CLI and the behaviour together. They go around it only for admin
bootstrap (`luxd admin`), for things no client can see (the database, a
host's disk, nftables), and for fault injection.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
import uuid

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import pytest

from env import BIN_DIR, Host, TestEnvironment


def _require(env: TestEnvironment, *binaries: str) -> None:
    missing = [b for b in binaries if not env.binaries.get(b)]
    if missing:
        pytest.skip(f"not built yet: {', '.join(missing)}")


@pytest.fixture(scope="session")
def env() -> TestEnvironment:
    """The environment run_tests.py set up."""
    return TestEnvironment.load()


@pytest.fixture(scope="session")
def hosts(env: TestEnvironment):
    return env.hosts


@pytest.fixture(scope="session")
def require(env: TestEnvironment):
    """`require("luxd", "lux-runner")` skips the test until those exist."""
    return lambda *b: _require(env, *b)


class CLIError(Exception):
    def __init__(self, args, code, stdout, stderr):
        super().__init__(f"lux {' '.join(args)} exited {code}\nstdout: {stdout}\nstderr: {stderr}")
        self.code, self.stdout, self.stderr = code, stdout, stderr


class Lux:
    """The lux CLI, as a tenant."""

    def __init__(self, env: TestEnvironment, api_key: str, tenant_id: str):
        self.env, self.api_key, self.tenant_id = env, api_key, tenant_id

    def _env(self) -> dict:
        return {**os.environ, "LUX_URL": self.env.luxd_url, "LUX_API_KEY": self.api_key, "HOME": "/nonexistent"}

    def run(self, *args: str, check: bool = True, input: str | None = None, timeout: float = 120) -> subprocess.CompletedProcess:
        p = subprocess.run([str(BIN_DIR / "lux"), *args], env=self._env(), input=input,
                           capture_output=True, text=True, timeout=timeout)
        if check and p.returncode != 0:
            raise CLIError(args, p.returncode, p.stdout, p.stderr)
        return p

    def json(self, *args: str, **kw):
        out = self.run(*args, "-o", "json", **kw).stdout
        return json.loads(out)

    def popen(self, *args: str) -> subprocess.Popen:
        return subprocess.Popen([str(BIN_DIR / "lux"), *args], env=self._env(),
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    # -- conveniences ---------------------------------------------------

    def submit(self, spec: dict, *args: str) -> str:
        """lux run -f - with a spec; returns the run id."""
        return self.run("run", "-f", "-", *args, input=json.dumps(spec)).stdout.strip()

    def get(self, run_id: str) -> dict:
        return self.json("get", run_id)

    def wait_state(self, run_id: str, *states: str, timeout: float = 90) -> dict:
        return self.json("wait", run_id, "--state", ",".join(states), "--timeout", f"{int(timeout)}s", timeout=timeout + 10)

    def logs(self, run_id: str, *args: str) -> str:
        return self.run("logs", run_id, *args).stdout

    def records(self, run_id: str, *args: str) -> list[dict]:
        out = self.run("logs", run_id, "-o", "json", *args).stdout
        return [json.loads(l) for l in out.splitlines() if l.strip()]

    def wait_output(self, run_id: str, text: str, timeout: float = 60) -> str:
        deadline = time.time() + timeout
        out = ""
        while time.time() < deadline:
            out = self.logs(run_id)
            if text in out:
                return out
            time.sleep(0.5)
        raise AssertionError(f"{text!r} not in output of {run_id} after {timeout}s:\n{out}")

    def wait_activity(self, run_id: str, activity: str, timeout: float = 60) -> dict:
        deadline = time.time() + timeout
        run = {}
        while time.time() < deadline:
            run = self.get(run_id)
            if run.get("activity") == activity:
                return run
            time.sleep(0.3)
        raise AssertionError(f"run {run_id} never became {activity}: {run.get('state')} {run.get('activity')}")


@pytest.fixture(scope="session")
def tenant_factory(env: TestEnvironment):
    def make(**kw) -> Lux:
        _require(env, "luxd", "lux")
        name = f"t{uuid.uuid4().hex[:8]}"
        args = ["create-tenant", "--name", name]
        for k, v in kw.items():
            args += [f"--{k.replace('_', '-')}", str(v)]
        t = env.luxd_admin(*args)
        return Lux(env, t["apiKey"], t["tenantId"])
    return make


@pytest.fixture
def lux(tenant_factory) -> Lux:
    """A fresh tenant, driven through the CLI. Per test, so tests cannot
    leak state into each other."""
    return tenant_factory()


class Runners:
    """Runners on the simulated hosts, for one tenant."""

    def __init__(self, env: TestEnvironment, lux: Lux):
        self.env, self.lux = env, lux
        self.procs: dict[str, subprocess.Popen] = {}
        self.tokens: dict[str, str] = {}

    def token(self, *args: str) -> str:
        return self.env.luxd_admin("create-host-token", "--tenant", self.lux.tenant_id, *args)["token"]

    def start(self, host: Host, *extra: str, token: str | None = None, wait: bool = True) -> Host:
        tok = token or self.tokens.get(host.name) or self.token()
        self.tokens[host.name] = tok
        self.procs[host.name] = host.start_runner(self.env, tok, *extra)
        if wait:
            self.wait_ready(host.name)
        return host

    def wait_ready(self, name: str, timeout: float = 30):
        deadline = time.time() + timeout
        while time.time() < deadline:
            hosts = self.lux.json("hosts", "ls")
            if any(h["name"] == name and h["state"] == "ready" for h in hosts):
                return
            proc = self.procs.get(name)
            if proc and proc.poll() is not None:
                raise AssertionError(f"runner on {name} exited {proc.returncode}; see its runner.log")
            time.sleep(0.2)
        raise AssertionError(f"host {name} did not become ready")

    def stop(self, host: Host, signal: str = "TERM"):
        host.stop_runner(signal)
        p = self.procs.pop(host.name, None)
        if p:
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()

    def stop_all(self):
        for name in list(self.procs):
            host = next(h for h in self.env.hosts if h.name == name)
            self.stop(host)


def cleanup_host(host: Host):
    """Remove everything lux left on a host, so tests do not see each
    other's containers, volumes or snapshots."""
    host.exec("sh", "-c",
              "podman rm -f -t 0 $(podman ps -aq --filter label=lux.managed=true) >/dev/null 2>&1; "
              "podman volume rm -f $(podman volume ls -q --filter label=lux.managed=true) >/dev/null 2>&1; "
              "podman network rm -f $(podman network ls -q --filter label=lux.managed=true) >/dev/null 2>&1; "
              "rm -rf /var/lib/lux; true", check=False)


@pytest.fixture
def runners(env: TestEnvironment, lux: Lux, require):
    require("luxd", "lux-runner", "lux-shim", "lux")
    r = Runners(env, lux)
    yield r
    r.stop_all()
    for h in env.hosts:
        if h.exec("sh", "-c", "docker inspect >/dev/null 2>&1; echo ok", check=False).strip() == "ok":
            cleanup_host(h)


@pytest.fixture
def fake_image(env: TestEnvironment, require) -> str:
    require("lux-fake")
    return env.fake_image


def generic(image: str, *cmd: str, **extra) -> dict:
    spec = {"image": {"ref": image}, "workload": {"adapter": "generic", "command": list(cmd)}}
    spec.update(extra)
    return spec


def fake_agent(image: str, prompt: str = "", **extra) -> dict:
    """A spec running lux-fake over ACP, with its transcript on a state volume."""
    spec = {
        "image": {"ref": image},
        "workload": {"adapter": "acp", "command": ["lux-fake"], "prompt": prompt, "workdir": "/workspace"},
        "volumes": [
            {"name": "workspace", "path": "/workspace", "kind": "state"},
            {"name": "home", "path": "/home/agent", "kind": "state"},
        ],
    }
    for k, v in extra.items():
        if isinstance(v, dict) and isinstance(spec.get(k), dict):
            spec[k] = {**spec[k], **v}
        else:
            spec[k] = v
    return spec
