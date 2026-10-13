# Spot Runs v1

## Contract and staged implementation

Spot is a Run scheduling class, independent of the host's EC2 market. A spot
spec defaults to `resumePolicy: never`; other explicit policies are invalid.
Output and artifacts retain the existing never-policy behavior.

This worktree implements a fail-closed foundation, not a usable v1. Spot
admission remains disabled until capacity aging and placement reclamation are
implemented together. No runner advertises the spot reclamation capability.

## Implementation checklist

- [x] RunSpec `spot`, normalized never policy, useful validation errors.
- [x] CLI `lux run --spot`, preserving explicit policy for server validation.
- [x] Server submission persistence and resume refusal tests.
- [x] Shared hostFit runner-capability refusal, including chosen hosts.
- [x] Exclude spot demand from fleet planner; verify ordinary demand unchanged.
- [x] Disable spot placement independently of capability until admission exists.
- [ ] Durable capacity age and normal-first admission across queue batches.
- [ ] Placement-local reclamation and atomic requester reservation.
- [ ] Runner immediate SIGTERM and persisted 15s SIGKILL deadline.
- [ ] Read-only submit validateOnly and CLI dry-run, effective shaped resources.
- [x] Targeted real PostgreSQL/race tests and six guard mutations.
- [ ] Reclamation concurrency, Linux runner and full server gates.

## Capacity-age representation for the remaining implementation

Keep ordinary allocation accounting in live placements. Add one durable host-local
age record containing a generation (runner connection/boot identity, capacity and
pool shape), an observed free vector (CPU, memory, disk and slots), and a bounded
transition history. Admission uses the component-wise minimum free vector over a
strictly greater-than-60s interval, including the current allocation. Every
allocation/release updates it under the existing sorted host advisory lock.
Missing observations, reconnect, process restart without continuity proof, shape
changes and history overflow conservatively invalidate aged capacity. Preserve
unchanged old holes when unrelated small allocations change the vector. Sampling
CPU load or dating only the current total free capacity is insufficient.

## Reclamation reservation for the remaining implementation

Use a durable host-local pledge keyed by trigger normal Run and a victim ledger
with a unique placement claim. Victim resource accounting stays in live placements
until actual release. Pledges subtract only released resources from free admission;
never subtract a still-live victim twice. Under Run-before-sorted-host lock order,
recheck hostFit with real tenancy and shaped resources after simulated removals.
Acquire all required Run locks before host locks, or restart the transaction;
never acquire victim Run locks while holding host locks.

Requester cancellation releases/transfers the pledge but cannot withdraw a notice
already delivered. Host disconnect/restart invalidates pledges and aged capacity;
reconciliation must wait for actual placement/disk cleanup before admission.
Runner first-receipt deadline is durable and cannot be extended by retries,
reconnects, hooks or workload grace. Use the existing stop lifecycle, not host
EC2 drain. A partial unique index on active victim placement claims and an index
on active pledges by host are needed; no schema migration is shipped by this
foundation.

Dry-run must share submit normalization and the eventual admission function,
perform only reads, report advisory live fit rather than pool-can-ever-fit, and
explicitly document races. It is not implemented here rather than returning a
fictional live-admission result.
