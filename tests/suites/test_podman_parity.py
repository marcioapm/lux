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

Every field either matches podman, or is in INTENDED (a difference lux
means, with the reason and the doc that says so), or is in GAPS (a
difference nobody has decided on yet). A difference in neither fails
test_matches_podman. Each gap is its own strict xfail: changing lux so
that a gap closes fails its test, and the gap then moves out of GAPS.
"""

from __future__ import annotations

import base64
import fnmatch
import json
import re

import pytest

from conftest import Runners, cleanup_host
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
p shmsize sh -c 'df -k /dev/shm | tail -1'
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

CONTEXTS = ["workload", "exec", "exec-tty", "server"]

DOCS = "https://github.com/marcioapm/lux/blob/main/docs/"

# Differences lux intends: (contexts, field patterns, why, the doc that
# says so). fnmatch patterns over the field names fields() makes.
INTENDED = [
    (CONTEXTS, ["pid"],
     "lux-shim is PID 1 (--init=false) and starts the workload, every exec and every server as its "
     "children; podman's PID 1 is the image's process, and podman exec's parent is outside the container",
     DOCS + "podman.md#containers"),
    (CONTEXTS, ["caps.CapBnd", "caps.NoNewPrivs"],
     "every Run drops all capabilities but CHOWN, DAC_OVERRIDE, FOWNER, SETUID, SETGID and KILL, and "
     "sets no-new-privileges (a nested Run keeps SYS_CHROOT and loses no-new-privileges)",
     DOCS + "podman.md#containers"),
    (CONTEXTS, ["resolv.nameserver"],
     "a Run's resolver is lux's DNS stub on its network's gateway (podman's DNS is off), so egress "
     "rules apply to names",
     DOCS + "podman.md#networks"),
    (CONTEXTS, ["env.LUX_*"],
     "lux's own variables (LUX_RUN_ID, LUX_EPOCH, LUX_CPUS, LUX_MEMORY_LIMIT, LUX_CPUSET, LUX_INPUTS, "
     "LUX_SERVICE_*); LUX_ is reserved, a spec's env cannot use it",
     DOCS + "runspec.md#rules"),
    (CONTEXTS, ["hostname", "env.HOSTNAME"],
     "every Run's hostname is `lux` (--hostname=lux), the same on every placement, rather than a "
     "container id that changes on each; lux exec and servers have the workload's environment, "
     "HOSTNAME included, where podman exec has none",
     DOCS + "podman.md#containers"),
    (CONTEXTS, ["cgroup"],
     "every Run has a private cgroup namespace (--cgroupns=private, whatever the host's "
     "containers.conf); with sandbox.cgroupDelegation the workload runs in /workload/main",
     DOCS + "podman.md#limits-of-nested-containers"),
    (["workload"], ["fds.0"],
     "a generic workload's stdin is a pipe from lux-shim: input to the Run is written to it",
     DOCS + "adapters.md#writing-a-workload-for-generic"),
]

# Differences nobody has decided on yet: name -> (contexts, field
# patterns, what differs). Each is a strict xfail in test_known_gap; the
# report on the branch that added this suite analyses each one.
GAPS = {
    "image-workdir": (CONTEXTS, ["pwd"],
                      "the image's WORKDIR is not the default working directory: lux starts in the "
                      "user's home when workload.workdir is unset"),
    "image-env-home": (CONTEXTS, ["env.HOME"],
                       "an image's ENV HOME is replaced by the passwd home"),
    "env-user": (CONTEXTS, ["env.USER"],
                 "lux sets USER to the workload user's name; podman sets no USER"),
    "image-entrypoint": (["workload"], ["entrypoint"],
                         "the image's ENTRYPOINT never runs: workload.command replaces both "
                         "ENTRYPOINT and CMD"),
    "pty-term": (["exec-tty"], ["env.TERM"],
                 "lux exec -t sets no TERM; podman exec -t sets TERM=xterm"),
}

# Fields podman's and lux's probes do not have in common by construction:
# PWD follows pwd, SHLVL counts the shells the ENTRYPOINT adds, and
# PROBE_ENTRYPOINT is the entrypoint field.
SKIP_ENV = {"PWD", "SHLVL", "PROBE_ENTRYPOINT"}


def spec(*cmd: str, **workload) -> dict:
    w = {"adapter": "generic", **workload}
    if cmd:
        w["command"] = list(cmd)
    return {"image": {"build": {"containerfile": CONTAINERFILE}}, "workload": w}


def probe_lines(text: str) -> list[tuple[str, str]]:
    """The probe's (field, line) pairs in text: the last complete probe. A
    terminal may echo input (a ^@) ahead of the first marker."""
    lines = text.replace("\r", "").splitlines()
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
            # "Max open files   1048576   1048576   files": name, soft, hard.
            add("limits." + line[:26].strip(), " ".join(line[26:].split()[:2]))
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
            # (podman's own DNS, which lux turns off) is not a search domain.
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


def _matches(context: str, key: str, entries) -> bool:
    return any(context in ctxs and any(fnmatch.fnmatchcase(key, p) for p in pats) for ctxs, pats, *_ in entries)


def intended(context: str, key: str) -> bool:
    return _matches(context, key, INTENDED)


def gap(context: str, key: str) -> str | None:
    return next((n for n, (ctxs, pats, _) in GAPS.items()
                 if context in ctxs and any(fnmatch.fnmatchcase(key, p) for p in pats)), None)


def stop_signal(text: str) -> str | None:
    found = re.findall(r"stop-signal=(\w+)", text)
    return found[-1] if found else None


@pytest.fixture(scope="module")
def probes(env, tenant_factory, require):
    """Every probe, once: {context: (podman fields, lux fields)}, and how
    each side was stopped. One Run and one podman container of its image,
    on the same host."""
    require("luxd", "lux-runner", "lux-shim", "lux")
    lux = tenant_factory()
    runners = Runners(env, lux)
    host = env.hosts[0]
    pname = ""
    try:
        runners.start(host)
        run_id = lux.submit(spec(*COMMAND))
        lux.wait_output(run_id, "waiting", timeout=300)
        image = lux.get(run_id)["image"]["imageId"]
        lux_name = "lux-" + run_id

        raw = {"workload": lux.logs(run_id),
               "exec": lux.run("exec", run_id, "-T", "--", "probe", input="").stdout,
               "exec-tty": lux.run("exec", run_id, "-t", "--", "probe", input="").stdout}
        lux.run("server", "add", run_id, "probe", "8080", "--", *SERVER)
        raw["server"] = wait_until(lambda: (lambda o: o if "probe-end" in o else None)(
            lux.logs(run_id, "--server", "probe")), 60, 0.5, "no server output")

        pname = "parity-" + run_id
        host.podman("run", "-d", "--name", pname, image, *COMMAND)
        logs = wait_until(lambda: (lambda o: o if "waiting" in o else None)(host.podman("logs", pname)),
                          60, 0.5, "podman container never waited")
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
        yield out
    finally:
        if pname:
            host.podman("rm", "-f", pname, check=False)
        runners.stop_all()
        cleanup_host(host)


def test_probe_sees_the_image(probes):
    """The image is what the comparison assumes: podman runs it as runner
    with both extra groups, in its WORKDIR, with its ENV and ENTRYPOINT,
    and stops it with its STOPSIGNAL."""
    podman, lux = probes["workload"]
    assert probes["stop"][0] == "QUIT", probes["stop"]
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
    """Every field is podman's, an intended difference, or a known gap."""
    podman, lux = probes[context]
    unknown = {k: v for k, v in diff(podman, lux).items() if not intended(context, k) and not gap(context, k)}
    assert not unknown, "differences from podman that are neither INTENDED nor in GAPS " \
                        "(field: (podman, lux)):\n" + "\n".join(f"  {k}: {v!r}" for k, v in unknown.items())


def _gap_params():
    return [pytest.param(name, marks=pytest.mark.xfail(strict=True, reason=GAPS[name][2]), id=name)
            for name in GAPS]


@pytest.mark.parametrize("name", _gap_params())
def test_known_gap(probes, name):
    """Passes once lux matches podman in this gap's fields (then move the
    gap out of GAPS). Each gap must still show somewhere, or it is stale."""
    contexts, pats, _ = GAPS[name]
    found = {ctx: {k: v for k, v in diff(*probes[ctx]).items() if any(fnmatch.fnmatchcase(k, p) for p in pats)}
             for ctx in contexts}
    found = {ctx: d for ctx, d in found.items() if d}
    assert not found, f"{name}: (podman, lux) per context: {found}"


@pytest.mark.xfail(strict=True, reason="gap image-stopsignal: lux stops a generic workload with SIGTERM "
                                       "whatever the image's STOPSIGNAL")
def test_gap_image_stopsignal(probes):
    podman, lux = probes["stop"]
    assert lux == podman


@pytest.mark.xfail(strict=True, reason="gap image-cmd: a generic spec must give workload.command; podman "
                                       "runs the image's ENTRYPOINT and CMD when no command is given")
def test_gap_image_cmd(env, lux, runners, hosts):
    p = lux.run("run", "-f", "-", check=False, input=json.dumps(spec()))
    assert p.returncode == 0, p.stderr
    # Accepted: it must run what `podman run IMAGE` runs, the ENTRYPOINT
    # with the CMD.
    run_id = p.stdout.strip()
    runners.start(hosts[0])
    got = fields(lux.wait_output(run_id, "probe-end", timeout=300), "lux", "lux-" + run_id)
    assert (got["argv"], got["entrypoint"]) == ("/usr/local/bin/probe from-cmd", "ran"), got


def test_intended_and_gaps_are_disjoint():
    """A field is either intended or a gap in a context, never both."""
    for ctxs, pats, why, doc in INTENDED:
        assert why and doc.startswith(DOCS), (pats, doc)
        for ctx in ctxs:
            for p in pats:
                assert gap(ctx, p) is None, (ctx, p)
