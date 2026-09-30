"""A luxd outage longer than the lease (a machine reboot, say) loses no Run
and no host: time no luxd could hear heartbeats is not held against them."""

from __future__ import annotations

import psycopg

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def ticks(lux, run_id: str) -> int:
    return lux.logs(run_id).count("tick-")


def test_a_running_run_survives_a_luxd_outage(env, lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", 'i=0; while true; do i=$((i+1)); echo "tick-$i"; sleep 1; done'))
    lux.wait_output(run_id, "tick-2")

    env.stop_luxd()
    # The outage, without waiting through it: an hour ago was the last any
    # luxd, host or lease was heard of. A luxd that held that against them
    # would lose them all at its first reap.
    with psycopg.connect(env.owner_dsn, autocommit=True) as conn:
        conn.execute("UPDATE luxd_alive SET at = now() - interval '1 hour'")
        conn.execute("UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE state IN ('ready', 'draining')")
        conn.execute("UPDATE placements SET lease_expires_at = now() - interval '1 hour' WHERE state = 'running'")
        restarted = conn.execute("SELECT now()").fetchone()[0]
    env.start_luxd()

    # The runner reaches the new luxd and renews: host and lease fresh again.
    def renewed():
        with psycopg.connect(env.owner_dsn) as conn:
            return conn.execute(
                """SELECT h.state = 'ready' AND h.last_heartbeat > %s AND p.state = 'running' AND p.lease_expires_at > now()
                   FROM placements p JOIN hosts h ON h.id = p.host_id WHERE p.run_id = %s AND p.state <> 'lost'""",
                (restarted, run_id)).fetchone()
    wait_until(lambda: (r := renewed()) and r[0], 60, 0.5, "the host never renewed its heartbeat and lease")

    run = lux.get(run_id)
    assert run["state"] == "running", run
    assert len(run["placements"]) == 1, run["placements"]
    # And the same container goes on.
    seen = ticks(lux, run_id)
    wait_until(lambda: ticks(lux, run_id) > seen, 30, 0.5, "no output after the outage")
    lux.run("cancel", run_id)
