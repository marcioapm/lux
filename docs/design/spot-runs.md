# Spot Runs v1

Spot is a Run class independent of host EC2 market. Spot normalizes to never,
refuses other resume policies, and retains existing never output/artifact behavior.

## Checklist

- [x] Spec/CLI opt-in, persistence, resume refusal, capability gate.
- [x] Queued spot demand excluded from planner and scale-up/empty-host protection.
- [x] Durable bounded capacity-age ledger, strict >60s, preserved small holes.
- [x] Shared spot admission with queued normal priority beyond first batch.
- [x] Single-host sufficient victim plan, durable claims/pledges, separate signal pass.
- [x] Targeted real PG concurrent scheduler/release test.
- [ ] Broader cancellation/host-loss/early-exit and reclamation guard mutations.
- [ ] Read-only submit validateOnly/CLI dry-run.
- [ ] Runner immediate SIGTERM, persisted 15s deadline; capability remains unadvertised.
- [ ] Live spot autoscaler/warm-while-active integration and Linux/full gates.

## Capacity ledger

Migration 072 adds host_capacity_age(host_id primary key, key, history). Up to 32
transitions carry CPU/memory/disk/slot free vectors and timestamps. Admission takes
the component-wise minimum of intervals intersecting the last 60s, requiring a
strictly older boundary. Overflow merges oldest intervals by minimum (understates
rather than invents capacity). One indexed read per host loads the history.
Ordinary resources still come from live placements; sampling CPU load is irrelevant.

Migration 073 triggers update the ledger on placement insert/delete/state/resource
changes, including release between scheduler ticks. Trigger locking uses cost-host.
Host hello bumps capacity_epoch; capacity, epoch, pool, state/draining and shape
changes delete history. A process id prefixes the history key, conservatively
resetting on another luxd process/restart. This can reduce utilization under HA;
it cannot age unknown capacity. Scheduler applies in-batch allocations immediately
and persists the minimum-compatible history. Tests inject s.now / pure timestamps.

## Claims and pledges

placements.reclaim_for is an irreversible claim (one scalar per placement).
spot_pledges(run_id primary key, host_id indexed, tenant_id, resources) holds the
normal Run's promised vector. Extra allocation is component-wise max(0, requested
minus claimed live victim usage); live victim resources remain counted once.
A normal cannot start until every claimed live victim releases, even if it could
otherwise fit. Simulated removals remove one real tenant entry per victim and
reuse hostFit with shaped effective resources. Youngest-first deterministic greedy
selection, then redundant-victim pruning, is polynomial, not subset search.

Scheduler already holds requester Run locks then sorted host advisory locks.
Victim placement claims use FOR UPDATE SKIP LOCKED, never victim Run locks.
After commit a separate signalReclaims transaction acquires each victim Run first
and reuses requestStop with spot-reclaim. Durable claims recover a crash between
claim and signal. Reclaim cause/event is placement-local, not host EC2 drain;
exit terminates with explicit cause. Cancellation deletes stale pledges on the
next eligible-host pass, but does not unclaim victims or withdraw notices.
Host state/reconnect changes delete pledges. No claim ever debits victim usage
before actual release. Broader cleanup/concurrency proof remains required.

Dry-run must use spotAdmission and submit normalization with no writes; normal
wouldStartNow means current hostFit, not reclaim/autolaunch/workload success.
Runner deadline enforcement is still outstanding; capability is not advertised.
