"""EC2 user data formats (internal/hostboot, internal/ec2's
template.userData): "ignition" (default, Fedora CoreOS), "script"
(cloud-init on a stock image), "env" (a custom AMI's own boot script). One
source of truth rendered three ways; each launches a working runner
against the fake EC2, and a bad value is refused when the pool is set."""

from __future__ import annotations

import json

import pytest

from conftest import CLIError, fake_only, generic
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2


@pytest.fixture(autouse=True)
def _clean(lux, ec2):
    yield
    lux.run("pools", "rm", "burst", check=False)
    wait_until(lambda: not ec2.running(), 90, 1, "the removed pool's instances were not terminated")


@pytest.mark.parametrize("user_data", ["ignition", "script", "env", None])
def test_a_host_boots_and_registers_in_every_user_data_format(lux, ec2, user_data):
    """Whichever format the pool's template names, the fake EC2's `_boot`
    reads the same env out of it (fake_ec2._parse_user_data) and the
    runner registers normally. None: the default (unset), which must
    behave as "ignition"."""
    fake_only(ec2)
    template = dict(ec2.template)
    if user_data is not None:
        template["userData"] = user_data
    lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(template), "--max", "1")
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "booted", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=120)
    assert "booted" in lux.logs(run_id)
    [inst] = ec2.running()
    # The env the fake parsed out of the rendered user data reached the
    # runner: it registered with the name luxd gave it.
    assert lux.get(run_id)["placements"][0]["hostName"] == inst["tags"]["Name"]


def test_an_unknown_user_data_format_is_refused_when_the_pool_is_set(lux, ec2):
    template = {**ec2.template, "userData": "cloud-init-yaml"}
    with pytest.raises(CLIError) as e:
        lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(template))
    assert "userData" in e.value.stderr, e.value.stderr
