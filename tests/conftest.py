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

from env import BIN_DIR, Host, TestEnvironment, wait_until


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
        return wait_until(lambda: (lambda out: out if text in out else None)(self.logs(run_id)),
                          timeout, 0.5, f"{text!r} not in output of {run_id}")

    def wait_activity(self, run_id: str, activity: str, timeout: float = 60) -> dict:
        return wait_until(lambda: (lambda r: r if r.get("activity") == activity else None)(self.get(run_id)),
                          timeout, 0.3, f"run {run_id} never became {activity}")

    def wait_uploaded(self, run_id: str, timeout: float = 30) -> dict:
        """Wait until the Run's latest snapshot is in S3."""
        return wait_until(lambda: (lambda sn: sn[-1] if sn and sn[-1]["uploaded"] else None)(self.json("snapshots", run_id)),
                          timeout, 0.5, f"snapshot of {run_id} never uploaded")


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

    def start(self, host: Host, *extra: str, token: str | None = None, wait: bool = True, name: str | None = None) -> Host:
        tok = token or self.tokens.get(host.name) or self.token()
        self.tokens[host.name] = tok
        self.procs[host.name] = host.start_runner(self.env, tok, *extra, name=name)
        if wait:
            self.wait_ready(name or host.name)
        return host

    def wait_ready(self, name: str, timeout: float = 30):
        def ready():
            for proc in self.procs.values():
                if proc.poll() is not None:
                    raise AssertionError(f"a runner exited {proc.returncode}; see its runner.log")
            return any(h["name"] == name and h["state"] == "ready" for h in self.lux.json("hosts", "ls"))
        wait_until(ready, timeout, 0.2, f"host {name} did not become ready")

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
        cleanup_host(h)


@pytest.fixture
def fake_image(env: TestEnvironment, require) -> str:
    require("lux-fake")
    return env.fake_image


def generic(image: str, *cmd: str, **extra) -> dict:
    spec = {"image": {"ref": image}, "workload": {"adapter": "generic", "command": list(cmd)}}
    spec.update(extra)
    return spec


AGENT_VOLUMES = [
    {"name": "workspace", "path": "/workspace", "kind": "state"},
    {"name": "home", "path": "/home/agent", "kind": "state"},
]


def fake_agent(image: str, prompt: str = "", adapter: str = "acp", **extra) -> dict:
    """A spec running lux-fake behind an adapter (acp, claude-code, codex:
    lux-fake speaks each protocol), with its transcript on a state volume."""
    spec = {
        "image": {"ref": image},
        "workload": {"adapter": adapter, "command": ["lux-fake"], "prompt": prompt, "workdir": "/workspace"},
        "volumes": [dict(v) for v in AGENT_VOLUMES],
    }
    for k, v in extra.items():
        if isinstance(v, dict) and isinstance(spec.get(k), dict):
            spec[k] = {**spec[k], **v}
        else:
            spec[k] = v
    return spec


# ---- agent harnesses --------------------------------------------------------

def pytest_generate_tests(metafunc):
    """Parameterize every test that takes `harness` over all harnesses:
    fake variants always, real ones as `agents`-marked cases."""
    if "harness" not in metafunc.fixturenames:
        return
    from harnesses import HARNESSES
    only = getattr(metafunc.function, "harness_filter", None)
    params = []
    for h in HARNESSES:
        if only and not only(h):
            continue
        params.append(pytest.param((h, False), id=f"{h.name}-fake"))
        if h.credentials:
            params.append(pytest.param((h, True), id=f"{h.name}-real", marks=pytest.mark.agents))
    metafunc.parametrize("harness", params, indirect=True)


@pytest.fixture
def harness(request, env: TestEnvironment):
    """An agent harness, fake or real (see tests/harnesses.py)."""
    from harnesses import Variant
    h, real = request.param
    if real:
        img = env.extra.get("images", {}).get(h.name)
        if not img:
            pytest.skip(f"no real {h.name}: set {', '.join(h.credentials)} and select -m agents")
        return Variant(h, True, img)
    _require(env, "lux-fake")
    return Variant(h, False, env.fake_image)


def harnesses(pred):
    """Decorator: run a harness-parameterized test only for harnesses
    matching pred (a capability check, e.g. lambda h: h.caps.steer_joins_turn)."""
    def mark(fn):
        fn.harness_filter = pred
        return fn
    return mark
