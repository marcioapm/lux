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
- [x] Read-only submit validateOnly/CLI dry-run sharing spotAdmission.
- [x] Runner/shim: immediate SIGTERM, no beforeStop, persisted first-receipt 15s
      deadline, runner-side container SIGKILL; capability advertised (Linux tests
      written, not run on Darwin).
- [ ] Live spot autoscaler/warm-while-active integration and Linux/full gates.

## Runner enforcement

luxd sends the existing stop message with reason spot-reclaim. The runner
persists runState.reclaimKillAt (first receipt + 15s; retries, reconnects and
re-adoption after a restart reuse the stored value), arms its own timer that
SIGKILLs the whole container with Podman at that instant (independent of luxd
and the shim), and sends the shim ShimStop with killAtUnixMs. The shim SIGTERMs
the workload's and init's process groups immediately, runs no beforeStop hook,
ignores workload grace, and schedules SIGKILL to the groups at the same instant.
Starting/building placements are cancelled through the existing cancelStart and
still get the container kill. Normal stops are unchanged. Exit snapshots follow
the never policy (no state-volume export; output/artifacts kept).

## Dry run

POST /v1/runs?validateOnly=true runs the full submit validation and shape
resolution, then, in a READ ONLY system transaction, the same candidateHosts and
spotAdmission the scheduler uses (normal Runs: hostFit only, with no reclamation
or launch promised). It writes nothing: no Run, event, secret cache, capacity
history, pledge or provider call. The answer is advisory and can be stale the
moment it returns; it is not a reservation.

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

