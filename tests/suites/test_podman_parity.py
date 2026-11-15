"""A lux workload's process environment against `podman run` of the same
image, field by field: the workload, `lux exec` without and with a
terminal, and a server, each against its podman counterpart (`podman run`
with the spec's command, `podman exec -i`, `podman exec -it`, `podman
exec`), plus how each is stopped.

The image declares everything a container's process environment can come
from: a USER with supplementary groups, a WORKDIR that is not the user's
home, ENV (PATH and HOME among it), an ENTRYPOINT and a CMD, a STOPSIGNAL,
and an /etc/passwd home and shell. One probe prints the process's view
(ids, umask, every rlimit, cwd, env, hostname, /etc/hosts, resolv.conf,
/tmp and /dev/shm, the workdir's and home's modes, capabilities,
no_new_privs, seccomp, cgroup, open fds, signal mask and ignored signals).

The reference is `podman run` with podman's stock namespace and cgroup
defaults given explicitly (PODMAN_DEFAULTS): a host's containers.conf may
change them, and the harness's hosts' does, so without them the reference
would share the host's network, IPC, UTS and cgroup namespaces while lux
sets its own (hardening() in internal/runner/placement.go).

Every field either matches podman, or is in INTENDED (a difference lux
means, with lux's value pinned, the reason and the doc that says so), or
is in GAPS (a difference nobody has decided on yet, with lux's current
value pinned). Anything else, including an INTENDED or GAPS field whose lux
value is not the pinned one, is unknown and fails test_matches_podman.
Each gap is its own strict xfail: changing lux so that a gap closes fails
its test, and the gap then moves out of GAPS. test_classify_flags_regressions
checks the classification itself on synthetic probes, with no environment.
"""

from __future__ import annotations

import base64
import fnmatch
import json
import re

import pytest

from conftest import Runners, cleanup_host, generic
from env import ALPINE_IMAGE, wait_until

# The probe: one "<field>\t<line>" per line of output, between markers, so
# the Run's log or a terminal's echo around it does not matter.
PROBE = r"""#!/bin/sh
p() { f=$1; shift; "$@" 2>&1 | while IFS= read -r l; do printf '%s\t%s\n' "$f" "$l"; done; }
echo probe-begin
p argv echo "$0 $*"
p entrypoint echo "${PROBE_ENTRYPOINT:-not-run}"
p pid echo "pid1=$([ $$ = 1 ] && echo self || { [ $PPID = 1 ] && echo parent || echo other; })"
p uid id -u
p gid id -g
p groups id -G
p umask umask
p limits tail -n +2 /proc/self/limits
p pwd pwd
p env env
p hostname hostname
p hostname_resolves sh -c 'getent hosts "$(hostname)" >/dev/null && echo yes || echo no'
p hosts cat /etc/hosts
p resolv cat /etc/resolv.conf
p tmpdirs stat -c '%n %a %U:%G' /tmp /dev/shm
p shmsize sh -c "df -k /dev/shm | awk 'NR==2{print \$2}'"
p workdir_mode stat -c '%n %a %U:%G' /srv/app
p home_mode stat -c '%n %a %U:%G' /home/runner
p caps grep -E '^(CapInh|CapPrm|CapEff|CapBnd|CapAmb|NoNewPrivs|Seccomp):' /proc/self/status
p cgroup cat /proc/self/cgroup
p fds ls -l /proc/$$/fd
p signals grep -E '^Sig(Blk|Ign):' /proc/self/status
p tty sh -c 'tty || true'
echo probe-end
"""

# The image's ENTRYPOINT: marks that it ran (PROBE_ENTRYPOINT), then runs
# its arguments.
ENTRY = """#!/bin/sh
export PROBE_ENTRYPOINT=ran
exec "$@"
"""

# Waits to be stopped, and says which signal stopped it.
WAIT = """#!/bin/sh
for s in HUP INT QUIT USR1 USR2 TERM; do trap "echo stop-signal=$s; exit 0" $s; done
echo waiting
while :; do sleep 1; done
"""


def _install(path: str, text: str) -> str:
    return f"echo {base64.b64encode(text.encode()).decode()} | base64 -d > {path} && chmod 755 {path}"


# HOME in ENV differs from the passwd home on purpose: which one wins is a
# field. PATH puts a directory first that lux's default PATH lacks.
CONTAINERFILE = f"""FROM {ALPINE_IMAGE}
RUN addgroup -g 4001 extra1 && addgroup -g 4002 extra2 \\
 && adduser -D -u 1000 -h /home/runner -s /bin/ash runner \\
 && addgroup runner extra1 && addgroup runner extra2 \\
 && mkdir -p /srv/app && chown runner:runner /srv/app && chmod 750 /srv/app && chmod 700 /home/runner \\
 && {_install("/usr/local/bin/probe", PROBE)} \\
 && {_install("/usr/local/bin/probe-entry", ENTRY)} \\
 && {_install("/usr/local/bin/probe-wait", WAIT)}
ENV PATH=/opt/probe/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \\
    HOME=/srv/home-from-env PROBE_IMAGE_ENV=from-image LANG=C.UTF-8
USER runner
WORKDIR /srv/app
ENTRYPOINT ["/usr/local/bin/probe-entry"]
CMD ["probe", "from-cmd"]
STOPSIGNAL SIGQUIT
"""

# The workload's command, given to lux as workload.command and to podman as
# `podman run IMAGE <command>` (which keeps the ENTRYPOINT and replaces
# CMD): probe once, then wait to be stopped.
COMMAND = ["sh", "-c", "probe from-spec; exec probe-wait"]
# A server's command, and podman's exec of it: probe as the shell's first
# command in both (busybox sh runs a lone command differently).
SERVER = ["sh", "-c", "probe; exec sleep 300"]
SERVER_PODMAN = ["sh", "-c", "probe; exit 0"]

# podman's stock defaults, given explicitly to the reference container. A
# host's containers.conf may change any of them (the harness's hosts' sets
# netns, ipcns, utsns and cgroupns to "host" and cgroups to "disabled"),
# which is why hardening() sets lux's explicitly too. userns is left to
# the host: podman's stock default for rootful podman is the host's user
# namespace, which is also what the hosts' containers.conf says, and no
# field the probe prints depends on the mapping (ids, modes and
# capabilities are the container's view either way); --userns=auto would
# mirror lux but not what `podman run` gives a user.
PODMAN_DEFAULTS = ["--network=bridge", "--uts=private", "--ipc=private", "--pid=private",
                   "--cgroupns=private", "--cgroups=enabled"]

CONTEXTS = ["workload", "exec", "exec-tty", "server"]

DOCS = "https://github.com/marcioapm/lux/blob/main/docs/"

# A pinned value that means "lux has no such field".
ABSENT = object()

IPV4 = r"\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}"

# Differences lux intends: (contexts, {field pattern: lux's value}, why,
# the doc that says so). Patterns are fnmatch patterns over the field names
# fields() makes; a value is a regex lux's whole value must match, or
# ABSENT. A pattern without wildcards is checked in every listed context
# whether or not podman differs (lux matching podman there is a
# regression too); a wildcard pattern is checked for every lux field it
# names, and the first pattern that names a field is the one used.
INTENDED = [
    # The workload's and a server's probe run under their `sh -c`, a child
    # of lux-shim: neither the probe nor its parent is PID 1. An exec's
    # probe is lux-shim's own child.
    (["workload", "server"], {"pid": r"pid1=other"},
     "lux-shim is PID 1 (--init=false) and starts the workload and every server as its children "
     "(here under their `sh -c`); podman's PID 1 is the image's process",
     DOCS + "podman.md#containers"),
    (["exec", "exec-tty"], {"pid": r"pid1=parent"},
     "lux exec runs its command as a child of lux-shim, PID 1; podman exec's parent is outside the "
     "container",
     DOCS + "podman.md#exec-attach-ports"),
    (CONTEXTS, {"caps.CapBnd": r"00000000000000eb", "caps.NoNewPrivs": r"1"},
     "every Run drops all capabilities but CHOWN, DAC_OVERRIDE, FOWNER, SETUID, SETGID and KILL, and "
     "sets no-new-privileges (a nested Run keeps SYS_CHROOT and loses no-new-privileges)",
     DOCS + "podman.md#containers"),
    (CONTEXTS, {"resolv.nameserver": rf"nameserver {IPV4}"},
     "a Run's resolver is lux's DNS stub on its network's gateway, its only nameserver (podman's DNS is "
     "off), so egress rules apply to names",
     DOCS + "podman.md#networks"),
    (CONTEXTS, {"env.LUX_RUN_ID": r"run_[a-z0-9]+", "env.LUX_EPOCH": r"[0-9]+", "env.LUX_*": r".+"},
     "lux's own variables (LUX_RUN_ID, LUX_EPOCH, LUX_CPUS, LUX_MEMORY_LIMIT, LUX_CPUSET, LUX_INPUTS, "
     "LUX_SERVICE_*); LUX_ is reserved, a spec's env cannot use it",
     DOCS + "runspec.md#rules"),
    (CONTEXTS, {"hostname": r"lux", "env.HOSTNAME": r"lux"},
     "every Run's hostname is `lux` (--hostname=lux), the same on every placement, rather than a "
     "container id that changes on each; lux exec and servers have the workload's environment, "
     "HOSTNAME included",
     DOCS + "podman.md#containers"),
    (["workload"], {"fds.0": r"\S+ pipe"},
     "a generic workload's stdin is a pipe from lux-shim: input to the Run is written to it",
     DOCS + "adapters.md#writing-a-workload-for-generic"),
]

# Differences nobody has decided on yet: name -> (contexts, {field
# pattern: lux's current value}, what differs), values as in INTENDED.
# A gap field with another lux value is unknown. Each is a strict xfail in
# test_known_gap.
GAPS = {
    "image-workdir": (CONTEXTS, {"pwd": r"/home/runner"},
                      "the image's WORKDIR is not the default working directory: lux starts in the "
                      "user's home when workload.workdir is unset"),
    "image-env-home": (CONTEXTS, {"env.HOME": r"/home/runner"},
                       "an image's ENV HOME is replaced by the passwd home"),
    "env-user": (CONTEXTS, {"env.USER": r"runner"},
                 "lux sets USER to the workload user's name; podman sets no USER"),
    "image-entrypoint": (["workload"], {"entrypoint": r"not-run"},
                         "the image's ENTRYPOINT never runs: workload.command replaces both "
                         "ENTRYPOINT and CMD"),
    "pty-term": (["exec-tty"], {"env.TERM": ABSENT},
                 "lux exec -t sets no TERM; podman exec -t sets TERM=xterm"),
}

# How lux stops a generic workload today, whatever the image's STOPSIGNAL
# (the image-stopsignal gap).
GAP_STOP_SIGNAL = "TERM"

# Fields podman's and lux's probes do not have in common by construction:
# PWD follows pwd, SHLVL counts the shells the ENTRYPOINT adds, and
# PROBE_ENTRYPOINT is the entrypoint field.
SKIP_ENV = {"PWD", "SHLVL", "PROBE_ENTRYPOINT"}


class HarnessError(Exception):
    """The harness could not get a probe: not a gap, so never an xfail."""


def spec(*cmd: str, **workload) -> dict:
    s = generic(ALPINE_IMAGE, *cmd)
    s["image"] = {"build": {"containerfile": CONTAINERFILE}}
    if not cmd:
        del s["workload"]["command"]
    s["workload"].update(workload)
    return s


def wait_text(fn, text: str, timeout: float, message: str) -> str:
    """fn() once it has text in it."""
    return wait_until(lambda: (lambda o: o if text in o else None)(fn()), timeout, 0.5, message)


def probe_lines(text: str) -> list[tuple[str, str]]:
    """The probe's (field, line) pairs in text: the last complete probe. A
    terminal may echo input (a ^@) ahead of the first marker."""
    lines = text.replace("\r", "").splitlines()
    assert any(l.strip() == "probe-end" for l in lines), f"no complete probe in:\n{text}"
    end = max(i for i, l in enumerate(lines) if l.strip() == "probe-end")
    begin = max(i for i, l in enumerate(lines[:end]) if l.strip().endswith("probe-begin"))
    return [tuple(l.split("\t", 1)) for l in lines[begin + 1:end] if "\t" in l]


def fields(text: str, hostname: str, name: str) -> dict[str, str]:
    """A probe's output as comparable fields. What names this one container
    (its name and address in /etc/hosts, pipe inodes, pty numbers, fd
    timestamps) is replaced by placeholders."""
    out: dict[str, list[str]] = {}

    def add(k: str, v: str) -> None:
        out.setdefault(k, []).append(v)

    for f, line in probe_lines(text):
        if f == "limits":
            # "Max open files            1048576              1048576              files":
            # columns are two or more spaces apart; name, soft, hard.
            name_, soft, hard = (re.split(r"\s{2,}", line.strip()) + ["", ""])[:3]
            add("limits." + name_, f"{soft} {hard}")
        elif f == "env":
            k, _, v = line.partition("=")
            if k not in SKIP_ENV:
                add("env." + k, v)
        elif f == "hosts":
            # The container's own entry, by its name: the name is a
            # placeholder, and its hostname is left out (podman
            # leaves it out too when the host's own /etc/hosts, which is
            # copied in, already has it; hostname_resolves says whether it
            # resolves). Every other line is the host's, the same for both.
            # Addresses other than loopback are placeholders too: the Run's
            # network is not podman's default one.
            words = line.split()
            if name in words:
                words = [words[0], *sorted("<name>" if w == name else w for w in words[1:] if w != hostname)]
            if words and not words[0].startswith(("127.", "::1")):
                words[0] = "<ip>"
            add("hosts", " ".join(words))
        elif f == "resolv":
            # Shape only: nameservers are their own field, and dns.podman
            # (podman's own DNS, which lux turns off) is not a search
            # domain.
            if line.startswith("nameserver"):
                add("resolv.nameserver", line)
            elif line.startswith("search"):
                words = [w for w in line.split() if w != "dns.podman"]
                if len(words) > 1:
                    add("resolv.search", " ".join(words))
            else:
                add("resolv", line)
        elif f in ("caps", "signals"):
            k, _, v = line.partition(":")
            add(f + "." + k, v.strip())
        elif f == "fds":
            m = re.match(r"\S+\s+\d+\s+\S+\s+\S+\s+\d+\s+.*?\s(\d+) -> (.*)$", line)
            if m:
                target = re.sub(r"^pipe:\[\d+\]$", "pipe", m.group(2))
                target = re.sub(r"^/dev/pts/\d+$", "/dev/pts", target)
                add("fds." + m.group(1), line.split()[0] + " " + target)
        elif f == "tty":
            add(f, re.sub(r"^/dev/pts/\d+$", "/dev/pts", line))
        else:
            add(f, line)
    return {k: "\n".join(sorted(v) if k == "hosts" else v) for k, v in out.items()}


def diff(podman: dict[str, str], lux: dict[str, str]) -> dict[str, tuple[str | None, str | None]]:
    return {k: (podman.get(k), lux.get(k)) for k in sorted(podman.keys() | lux.keys()) if podman.get(k) != lux.get(k)}


def _pinned(pins: dict, key: str):
    """The value pins give key, or None when no pattern names it."""
    return next((v for p, v in pins.items() if fnmatch.fnmatchcase(key, p)), None)


def _holds(want, value: str | None) -> bool:
    if want is ABSENT:
        return value is None
    return value is not None and re.fullmatch(want, value) is not None


def _matches(context: str, key: str, entries) -> object | None:
    """The pinned value of the first entry for context naming key."""
    for ctxs, pins, *_ in entries:
        if context in ctxs and (want := _pinned(pins, key)) is not None:
            return want
    return None


def intended(context: str, key: str):
    return _matches(context, key, INTENDED)


def gap(context: str, key: str) -> str | None:
    """The name of the gap naming key in context."""
    return next((n for n, e in GAPS.items() if _matches(context, key, [e]) is not None), None)


def classify(context: str, podman: dict[str, str], lux: dict[str, str]) -> dict[str, tuple]:
    """Every unknown difference: field -> (podman, lux). A field is unknown
    when it differs from podman and is neither INTENDED nor a gap, or when
    it is INTENDED or a gap and lux's value is not the pinned one. Exact
    INTENDED fields are checked even where lux matches podman."""
    keys = set(diff(podman, lux)) | set(lux)
    keys |= {p for ctxs, pins, *_ in INTENDED if context in ctxs for p in pins if not _wild(p)}
    unknown = {}
    for k in sorted(keys):
        want = intended(context, k)
        if want is None:
            if podman.get(k) == lux.get(k):
                continue
            name = gap(context, k)
            want = _matches(context, k, [GAPS[name]]) if name else None
        if want is None or not _holds(want, lux.get(k)):
            unknown[k] = (podman.get(k), lux.get(k))
    return unknown


def _wild(p: str) -> bool:
    return any(c in p for c in "*?[")


def overlaps(keys) -> list[tuple[str, str]]:
    """(context, field) pairs both INTENDED and a gap, among keys and every
    exact field either list names."""
    names = set(keys)
    for ctxs, pins, *_ in [*INTENDED, *GAPS.values()]:
        names |= {p for p in pins if not _wild(p)}
    return [(ctx, k) for ctx in CONTEXTS for k in sorted(names)
            if intended(ctx, k) is not None and gap(ctx, k) is not None]


def stop_signal(text: str) -> str | None:
    found = re.findall(r"stop-signal=(\w+)", text)
    return found[-1] if found else None


@pytest.fixture(scope="module")
def site(env, tenant_factory, require):
    """The module's tenant and one runner on hosts[0], shared by every test
    here: (lux, host)."""
    require("luxd", "lux-runner", "lux-shim", "lux")
    lux = tenant_factory()
    runners = Runners(env, lux)
    host = env.hosts[0]
    try:
        try:
            runners.start(host)
        except AssertionError as e:
            raise HarnessError(f"runner on {host.name}: {e}") from e
        yield lux, host
    finally:
        runners.stop_all()
        cleanup_host(host)


@pytest.fixture(scope="module")
def probes(site):
    """Every probe, once: {context: (podman fields, lux fields)}, and how
    each side was stopped. One Run and one podman container of its image,
    on the same host."""
    lux, host = site
    pname = ""
    try:
        run_id = lux.submit(spec(*COMMAND))
        lux.wait_output(run_id, "waiting", timeout=300)
        image = lux.get(run_id)["image"]["imageId"]
        lux_name = "lux-" + run_id

        raw = {"workload": lux.logs(run_id),
               "exec": lux.run("exec", run_id, "-T", "--", "probe", input="").stdout,
               "exec-tty": lux.run("exec", run_id, "-t", "--", "probe", input="").stdout}
        lux.run("server", "add", run_id, "probe", "8080", "--", *SERVER)
        raw["server"] = wait_text(lambda: lux.logs(run_id, "--server", "probe"), "probe-end", 60, "no server output")

        pname = "parity-" + run_id
        host.podman("run", "-d", "--name", pname, *PODMAN_DEFAULTS, image, *COMMAND)
        logs = wait_text(lambda: host.podman("logs", pname), "waiting", 60, "podman container never waited")
        # podman exec -i: a stdin pipe, as lux exec -T gives; without -i,
        # /dev/null, as a server's.
        want = {"workload": logs,
                "exec": host.exec("podman", "exec", "-i", pname, "probe", input=b""),
                "exec-tty": host.exec("podman", "exec", "-it", pname, "probe", input=b""),
                "server": host.podman("exec", pname, *SERVER_PODMAN)}

        out = {}
        for ctx in CONTEXTS:
            pm_host = dict(probe_lines(want[ctx])).get("hostname", "")
            out[ctx] = (fields(want[ctx], pm_host, pname), fields(raw[ctx], "lux", lux_name))

        lux.run("stop", run_id, "--wait")
        host.podman("stop", pname)
        out["stop"] = (stop_signal(host.podman("logs", pname)), stop_signal(lux.logs(run_id)))
    except AssertionError as e:
        # A wait that timed out, or a probe that never completed.
        raise HarnessError(str(e)) from e
    finally:
        if pname:
            host.podman("rm", "-f", pname, check=False)
    yield out


def test_probe_sees_the_image(probes):
    """The image is what the comparison assumes: podman runs it as runner
    with both extra groups, in its WORKDIR, with its ENV and ENTRYPOINT,
    and stops it with its STOPSIGNAL; lux stops it with a signal the probe
    saw."""
    podman, lux = probes["workload"]
    assert probes["stop"][0] == "QUIT", probes["stop"]
    assert probes["stop"][1] in (GAP_STOP_SIGNAL, probes["stop"][0]), probes["stop"]
    assert podman["groups"] == "1000 4001 4002", podman
    assert podman["pwd"] == "/srv/app", podman
    assert podman["env.PROBE_IMAGE_ENV"] == "from-image", podman
    assert podman["entrypoint"] == "ran", podman
    assert lux["argv"] == podman["argv"] == "/usr/local/bin/probe from-spec", (lux, podman)
    for ctx in CONTEXTS:
        podman, lux = probes[ctx]
        assert len(podman) > 40 and len(lux) > 40, (ctx, podman, lux)


@pytest.mark.parametrize("context", CONTEXTS)
def test_matches_podman(probes, context):
    """Every field is podman's, an intended difference, or a known gap,
    with lux's pinned value."""
    podman, lux = probes[context]
    assert not overlaps(podman.keys() | lux.keys())
    unknown = classify(context, podman, lux)
    assert not unknown, "differences from podman that are neither INTENDED nor in GAPS with lux's " \
                        "pinned value (field: (podman, lux)):\n" + "\n".join(f"  {k}: {v!r}" for k, v in unknown.items())


def _gap_params():
    return [pytest.param(name, marks=pytest.mark.xfail(strict=True, raises=AssertionError, reason=GAPS[name][2]),
                         id=name)
            for name in GAPS]


@pytest.mark.parametrize("name", _gap_params())
def test_known_gap(probes, name):
    """Passes once lux matches podman in this gap's fields (then move the
    gap out of GAPS). Each gap must still show somewhere, or it is stale.
    Only the gap itself is an AssertionError: a field that is neither
    podman's nor the pinned value fails outright."""
    found = gap_shows(name, probes)
    assert not found, f"{name}: (podman, lux) per context: {found}"


def gap_shows(name: str, probes) -> dict:
    """Where gap name still shows: {context: {field: (podman, lux)}}. A
    gap field that is neither podman's nor the pinned value fails here, as
    pytest's Failed, not an AssertionError."""
    contexts, pins, _ = GAPS[name]
    found = {}
    for ctx in contexts:
        podman, lux = probes[ctx]
        for k, v in diff(podman, lux).items():
            want = _pinned(pins, k)
            if want is None:
                continue
            if not _holds(want, v[1]):
                pytest.fail(f"{name}: {ctx} {k} is neither podman's nor the gap's: (podman, lux) {v!r}")
            found.setdefault(ctx, {})[k] = v
    return found


@pytest.mark.xfail(strict=True, raises=AssertionError,
                   reason="gap image-stopsignal: lux stops a generic workload with SIGTERM whatever the "
                          "image's STOPSIGNAL")
def test_gap_image_stopsignal(probes):
    podman, lux = probes["stop"]
    if lux not in (GAP_STOP_SIGNAL, podman):
        pytest.fail(f"lux stopped the workload with {lux!r}, neither podman's {podman!r} nor {GAP_STOP_SIGNAL}")
    assert lux == podman


@pytest.mark.xfail(strict=True, raises=AssertionError,
                   reason="gap image-cmd: a generic spec must give workload.command; podman runs the image's "
                          "ENTRYPOINT and CMD when no command is given")
def test_gap_image_cmd(site):
    lux, _host = site
    p = lux.run("run", "-f", "-", check=False, input=json.dumps(spec()))
    if p.returncode != 0 and "command" not in p.stderr:
        pytest.fail(f"refused for another reason than the missing command: {p.stderr}")
    assert p.returncode == 0, p.stderr
    # Accepted: it must run what `podman run IMAGE` runs, the ENTRYPOINT
    # with the CMD.
    run_id = p.stdout.strip()
    try:
        out = lux.wait_output(run_id, "probe-end", timeout=300)
    except AssertionError as e:
        raise HarnessError(str(e)) from e
    got = fields(out, "lux", "lux-" + run_id)
    assert (got["argv"], got["entrypoint"]) == ("/usr/local/bin/probe from-cmd", "ran"), got


def test_classify_lists_are_disjoint():
    """A field is either intended or a gap in a context, never both, among
    the fields a probe has; each entry says why, and each INTENDED entry
    names its doc."""
    for ctxs, pins, why, doc in INTENDED:
        assert why and pins and doc.startswith(DOCS), (pins, doc)
    for name, (ctxs, pins, why) in GAPS.items():
        assert why and pins, name
    assert not overlaps([*_PODMAN, *_LUX["workload"]])


# ---- the classification itself, on synthetic probes --------------------------

# A podman probe and lux's of the same image, as fields() makes them: every
# field the regressions below touch, and a few that match.
_PODMAN = {
    "argv": "/usr/local/bin/probe from-spec", "entrypoint": "ran", "pid": "pid1=parent",
    "uid": "1000", "gid": "1000", "groups": "1000 4001 4002", "umask": "0022",
    "limits.Max open files": "1048576 1048576", "limits.Max processes": "unlimited unlimited",
    "pwd": "/srv/app", "env.HOME": "/srv/home-from-env", "env.PATH": "/opt/probe/bin:/usr/bin:/bin",
    "env.HOSTNAME": "0123456789ab", "env.LANG": "C.UTF-8",
    "hostname": "0123456789ab", "hostname_resolves": "yes",
    "resolv.nameserver": "nameserver 10.88.0.1", "resolv.search": "search example.test",
    "shmsize": "64000", "caps.CapInh": "0000000000000000", "caps.CapPrm": "0000000000000000",
    "caps.CapEff": "0000000000000000", "caps.CapBnd": "00000000800405fb", "caps.CapAmb": "0000000000000000",
    "caps.NoNewPrivs": "0", "caps.Seccomp": "2", "cgroup": "0::/",
    "fds.0": "lr-x------ pipe", "fds.1": "l-wx------ pipe", "fds.2": "l-wx------ pipe",
    "signals.SigBlk": "0000000000000000", "signals.SigIgn": "0000000000000000", "tty": "not a tty",
}
_LUX = {"workload": {
    **_PODMAN, "entrypoint": "not-run", "pid": "pid1=other", "pwd": "/home/runner",
    "env.HOME": "/home/runner", "env.USER": "runner", "env.HOSTNAME": "lux", "hostname": "lux",
    "env.LUX_RUN_ID": "run_0123456789abcdef", "env.LUX_EPOCH": "1", "env.LUX_CPUS": "1",
    "resolv.nameserver": "nameserver 10.89.0.1", "caps.CapBnd": "00000000000000eb", "caps.NoNewPrivs": "1",
}}

# Each is one way lux could regress, applied to lux's workload probe; each
# must be unknown. A value of None removes the field.
REGRESSIONS = {
    "umask": {"umask": "0077"},
    "dropped-group": {"groups": "1000 4001"},
    "rlimit": {"limits.Max open files": "1024 1048576"},
    "leaked-fd": {"fds.3": "lr-x------ /.lux/run/config.json"},
    "signal-mask": {"signals.SigBlk": "0000000000010000"},
    "ignored-signal": {"signals.SigIgn": "0000000000000004"},
    "capbnd-added": {"caps.CapBnd": "00000000000000ef"},
    "capbnd-podmans": {"caps.CapBnd": "00000000800405fb"},
    "capeff-raised": {"caps.CapEff": "00000000000000eb"},
    "no-new-privs-off": {"caps.NoNewPrivs": "0"},
    "cwd": {"pwd": "/tmp"},
    "cwd-podmans": {"pwd": "/"},
    "home": {"env.HOME": "/root"},
    "user": {"env.USER": "root"},
    "hostname": {"hostname": "lux-run_0123456789abcdef"},
    "hostname-podmans": {"hostname": "0123456789ab"},
    "env-hostname": {"env.HOSTNAME": "other"},
    "cgroup-path": {"cgroup": "0::/workload/main"},
    "second-nameserver": {"resolv.nameserver": "nameserver 10.89.0.1\nnameserver 8.8.8.8"},
    "nameserver-not-ipv4": {"resolv.nameserver": "nameserver fd00::1"},
    "run-id": {"env.LUX_RUN_ID": "oops"},
    "run-id-missing": {"env.LUX_RUN_ID": None},
    "lux-var-empty": {"env.LUX_CPUS": ""},
    "pid1": {"pid": "pid1=self"},
    "stdin-not-pipe": {"fds.0": "lrwx------ /dev/null"},
    "entrypoint": {"entrypoint": "maybe"},
    "uid": {"uid": "0"},
    "new-env": {"env.SECRET": "x"},
}


def _regressed(changes: dict) -> dict:
    lux = dict(_LUX["workload"])
    for k, v in changes.items():
        if v is None:
            lux.pop(k, None)
        else:
            lux[k] = v
    return lux


def test_classify_accepts_the_baseline():
    assert classify("workload", _PODMAN, _LUX["workload"]) == {}


@pytest.mark.parametrize("name", list(REGRESSIONS))
def test_classify_flags_regressions(name):
    """Each regression is an unknown difference, in the field it touches."""
    changes = REGRESSIONS[name]
    unknown = classify("workload", _PODMAN, _regressed(changes))
    assert set(changes) <= set(unknown), (name, unknown)


def _synthetic_probes(lux: dict) -> dict:
    tty_podman = {**_PODMAN, "env.TERM": "xterm"}
    return {ctx: (tty_podman if ctx == "exec-tty" else _PODMAN, lux) for ctx in CONTEXTS}


@pytest.mark.parametrize("name", list(GAPS))
def test_classify_gap_shows_until_closed(name):
    """Each gap shows on today's lux, and no longer once lux matches
    podman (test_known_gap then XPASSes); a third value fails outright."""
    assert gap_shows(name, _synthetic_probes(_LUX["workload"]))
    closed = {ctx: (p, {**p, **{k: v for k, v in _LUX["workload"].items() if gap(ctx, k) != name}})
              for ctx, (p, _) in _synthetic_probes({}).items()}
    assert not gap_shows(name, closed)
    _ctxs, pins, _ = GAPS[name]
    other = {**_LUX["workload"], **{k: "something-else" for k in pins}}
    with pytest.raises(pytest.fail.Exception):
        gap_shows(name, _synthetic_probes(other))
