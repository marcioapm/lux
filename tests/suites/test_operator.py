"""Operators: one key that sees and acts on every tenant, through the same
API and CLI as tenants. Tenant keys must never notice it exists."""

from __future__ import annotations

import pytest

from conftest import CLIError, Runners, generic
from env import ALPINE_IMAGE


def test_operator_sees_every_tenant_and_can_narrow(operator, tenant_factory, env):
    a, b = tenant_factory(), tenant_factory()
    ra = a.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    rb = b.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    names = {t["id"]: t["name"] for t in operator.json("tenants", "ls")}
    runs = {r["id"]: r for r in operator.json("ls", "--limit", "1000")}
    assert runs[ra]["tenant"] == names[a.tenant_id] and runs[rb]["tenant"] == names[b.tenant_id]
    # --tenant narrows to one tenant, by name or by id.
    only_a = {r["id"] for r in operator.json("ls", "--tenant", names[a.tenant_id])}
    assert ra in only_a and rb not in only_a
    assert {r["id"] for r in operator.json("ls", "--tenant", a.tenant_id)} == only_a
    # The TENANT column shows when rows span tenants.
    assert "TENANT" in operator.run("ls").stdout
    # A Run's own routes work without naming its tenant.
    assert operator.get(rb)["tenant"] == names[b.tenant_id]
    operator.run("cancel", rb)
    assert b.wait_state(rb, "cancelled")["state"] == "cancelled"
    a.run("cancel", ra)


def test_tenant_keys_stay_in_their_tenant(tenant_factory):
    a, b = tenant_factory(), tenant_factory()
    rb = b.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    # ?tenant= means nothing to a tenant key: not another tenant's Runs...
    assert rb not in {r["id"] for r in a.json("ls", "--tenant", b.tenant_id)}
    with pytest.raises(CLIError) as e:
        a.run("get", rb, "--tenant", b.tenant_id)
    assert e.value.code == 3
    # ...and no tenant list at all.
    with pytest.raises(CLIError) as e:
        a.run("tenants", "ls")
    assert "operator" in e.value.stderr
    assert a.json("status")["runs"] == {}
    b.run("cancel", rb)


def test_operator_must_name_a_tenant_to_create(operator, tenant_factory):
    t = tenant_factory()
    with pytest.raises(CLIError) as e:
        operator.submit(generic(ALPINE_IMAGE, "true"))
    assert "tenant" in e.value.stderr
    run_id = operator.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}), "--tenant", t.tenant_id)
    # Created in that tenant: its own key sees it.
    assert t.get(run_id)["id"] == run_id
    operator.run("cancel", run_id)


def test_operator_hosts_status_and_drain(operator, tenant_factory, env, hosts):
    a, b = tenant_factory(), tenant_factory()
    ra, rb = Runners(env, a), Runners(env, b)
    try:
        # Two tenants' hosts with the same name.
        ra.start(hosts[0], name="op-host")
        rb.start(hosts[1], name="op-host")
        names = {t["id"]: t["name"] for t in operator.json("tenants", "ls")}
        mine = [h for h in operator.json("hosts", "ls") if h["name"] == "op-host"]
        assert sorted(h["tenant"] for h in mine) == sorted([names[a.tenant_id], names[b.tenant_id]])
        # The name alone is ambiguous for an operator; --tenant or the id is not.
        with pytest.raises(CLIError) as e:
            operator.run("hosts", "get", "op-host")
        assert "more than one host" in e.value.stderr
        host_b = operator.json("hosts", "get", "op-host", "--tenant", b.tenant_id)
        assert host_b["tenant"] == names[b.tenant_id]

        run_b = b.submit(generic(ALPINE_IMAGE, "sleep", "300", resources={"cpus": 0.5}))
        b.wait_state(run_b, "running")
        h = operator.json("hosts", "get", host_b["id"])
        assert [p["runId"] for p in h["placements"]] == [run_b]
        assert h["allocated"]["cpus"] == 0.5
        # The Run is counted in its tenant's status, and in the system's.
        assert operator.json("status", "--tenant", b.tenant_id)["runs"]["running"] == 1
        assert operator.json("status")["runs"]["running"] >= 1
        assert "running" not in a.json("status")["runs"]
        # The operator drains another tenant's host: its Run is stopped
        # (no other host to go to).
        operator.run("hosts", "drain", host_b["id"])
        assert b.wait_state(run_b, "stopped", "resuming")["state"] in ("stopped", "resuming")
        assert b.json("hosts", "get", "op-host")["draining"]
        assert not a.json("hosts", "get", "op-host")["draining"]
        b.run("cancel", run_b)
    finally:
        ra.stop_all()
        rb.stop_all()
