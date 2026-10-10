"""EC2 pools, block storage: a launched host's own disk is recorded from
DescribeVolumes and priced from the Pricing API (both the fake's), and a
Run on it gets a Block storage line next to its compute one, shared by the
same reservation."""

from __future__ import annotations

from decimal import Decimal

import pytest

from conftest import FAKE_EC2_TIMERS, fake_only, generic
from ec2_helpers import _clean, ec2_hosts, pool  # noqa: F401 (_clean is an autouse fixture)
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2

# The fake's root disk (gp3, 100 GiB, 3000 IOPS, 125 MiB/s) at the real
# eu-north-1 list price: 100 × 0.0836 / 730 per hour, rounded to 9 digits.
GP3_100_PER_HOUR = "0.011452055"


@pytest.fixture
def priced(env, ec2):
    """luxd as the ec2 fixture starts it, also pricing from the fake's
    Pricing API, with second-scale cost ticks and price refreshes."""
    fake_only(ec2)
    env.stop_luxd()
    env.start_luxd(LUX_EC2_ENDPOINT=ec2.url, LUX_PRICING_ENDPOINT=ec2.url, AWS_ACCESS_KEY_ID="fake",
                   AWS_SECRET_ACCESS_KEY="fake", AWS_REGION="us-east-1", LUX_COSTS_EVERY="5s",
                   LUX_COSTS_PRICES_REFRESH="5s", **FAKE_EC2_TIMERS)
    return ec2


def test_a_launched_hosts_disk_is_its_runs_block_storage(lux, priced):
    pool(lux, priced, template={**priced.template, "region": "eu-north-1"}, max=1, min=1)
    [host] = wait_until(lambda: ec2_hosts(lux) or None, 120, 0.5, "the pool's host never registered")
    host = wait_until(lambda: (h := next(x for x in ec2_hosts(lux) if x["id"] == host["id"])).get("volumes") and h,
                      60, 0.5, "the host's volumes were never recorded")
    assert host["volumes"] == [{"type": "gp3", "sizeGiB": 100, "iops": 3000, "throughputMiBps": 125}], host["volumes"]
    assert any(c.get("volumeApiName") == "gp3" and c.get("regionCode") == "eu-north-1" for c in priced.pricing_calls)

    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "3", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=120)

    def final_cost():
        c = lux.json("cost", run_id)
        return c if c["status"] == "final" else None
    cost = wait_until(final_cost, 120, 1, "the Run's cost never became final")
    by_family = {line["family"]: line for line in cost["lines"]}
    assert set(by_family) == {"compute", "block-storage"}, cost["lines"]
    bs, compute = by_family["block-storage"], by_family["compute"]
    assert bs["source"] == "compute" and bs["item"] == "gp3:100GiB" and bs["final"], bs
    [p] = bs["details"]["placements"]
    [pc] = compute["details"]["placements"]
    assert p["ratePerHour"] == GP3_100_PER_HOUR and p["hostId"] == host["id"], p
    # The disk is shared by the same reservation, over the same window, as the machine.
    assert p["share"] == pc["share"] and p["from"] == pc["from"] and p["to"] == pc["to"], (p, pc)
    expected = Decimal(compute["amount"]) * Decimal(GP3_100_PER_HOUR) / Decimal(pc["ratePerHour"])
    assert abs(Decimal(bs["amount"]) - expected) <= Decimal("0.000000002"), (bs["amount"], expected)
    assert Decimal(bs["amount"]) > 0
    families = {f["family"]: f.get("displayName") for f in cost["byFamily"]}
    assert families == {"compute": "Compute", "block-storage": "Block storage"}, cost["byFamily"]
    # lux cost names the family as luxd does.
    assert "Block storage (block-storage)" in lux.run("cost", run_id).stdout
