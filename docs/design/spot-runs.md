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
- [x] Autoscaler: spot-only hosts are idle; scale-down reclaims their spot Runs.
- [ ] Linux/full gates (parent).

## Autoscaler

Spot placements never keep a host or pool alive. A ready host whose live
placements are all spot counts as idle (poolState), a spot placement does not
make a pool active for warm_while_active, and its end does not restart the
host's idle clock (last_placement_ended_at). The planner treats spot usage as
free (reclaimable), so spot work never causes a launch. drainForScaleDown locks
the host's spot Runs (Runs before hosts), drains the host only if it still has
no normal placement, and sends each spot placement the spot-reclaim stop: the
same irreversible SIGTERM/15s-SIGKILL path, ending the Run terminated with
"spot capacity reclaimed for a normal Run" — never a drain stop that would
resume it. The drained host is terminated once its placements are gone.

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

Migration 073's placement trigger appends a transition on every insert, delete,
resize, or entry/exit of the live states (a WHEN clause skips other updates),
including releases between scheduler passes, by any luxd. It takes no lock of its
own. The history row is written only under the host's cost-host advisory lock,
step 3 of the documented order (infraevents.go): every production writer of a
placement's state or resources already holds it: the scheduler (assign, the
insert), placementExited and placementLost (the only exits from the live
states), and the reapers that call them. Transitions within the live states
(assigned, starting, running, stopping) and stop fields do not fire it. The trigger
checks pg_locks for that lock; a write without it (no luxd path; test fixtures)
deletes the history instead, which only delays spot work. Tests run a scheduler
pass concurrently with exit reports, requestStop and reapLeases on one host.

The history key is capacity_epoch (bumped on every runner hello: reconnect or
restart), capacity, pool id and the pool's shape. It names no luxd process, so
several live luxd instances and restarts share and extend one history. A
mismatched key (hello, capacity, pool or shape change) starts over on read.
Pledges record the capacity_epoch they were made under; a hello voids them.

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

