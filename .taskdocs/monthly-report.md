# Monthly Cost Single Pass

Base: `ae656cbb6dd060e0058fe89c701c494629842fa3`. Branch: `monthly-cost-single-pass`.

## Baseline Saved Before Implementation

Local PostgreSQL 18.6 alpine, linux/arm64, Go 1.27.3. Isolated container
`lux-monthly-pg`, stock shared_buffers 128MB, jit on, work_mem 4MB.
No production, AWS, deployment, memory-branch configuration, or remote changes.
No AGENTS.md or .ai directory exists in this checkout.

Fixture: 216,000 hourly cost rows, 27,000 additional Runs, 30 days, two
tenants, two pools, migrated service-account ownership, eight families and two
currencies. Run spec contains about 1.4KB of distinct metadata plus labels.
Window fixed at 2026-08-27T10:00:00Z through 2026-09-26T10:00:00Z.
The 24-hour cases use the same start and fixture.

Before touching costread.go, `TestCostMonthlyExperiment` captured the real
HTTP handler's two WITH statements, including all 17 typed parameters and
current privacy predicates. Candidate derived from captured series SQL by
replacing only aggregation with grouping sets, using explicit grouping().
Each comparison replays totals, series and candidate under lux_app (verified
neither superuser nor BYPASSRLS), with explicit PREPARE parameter types.
All ordered full rows matched, including NULL groups and discriminator.

Warm EXPLAIN ANALYZE execution medians, three attempts; milliseconds:

| Window / Bucket | Mode / Work Mem | Totals | Series | Single Pass | Buffers Before / After |
| --- | --- | ---: | ---: | ---: | ---: |
| 24h / hour | custom / 4MB | 90.806 | 92.401 | 95.592 | 189912 / 94956 |
| 24h / day | custom / 4MB | 91.039 | 92.262 | 93.961 | 189912 / 94956 |
| 30d / hour | custom / 4MB | 293.466 | 353.433 | 538.013 | 607512 / 303756 |
| 30d / day | custom / 4MB | 292.254 | 341.514 | 445.994 | 607512 / 303756 |
| 30d / hour | custom / 16MB | 270.023 | 327.337 | 495.028 | 607512 / 303756 |
| 30d / day | custom / 16MB | 270.095 | 315.879 | 420.083 | 607512 / 303756 |
| 30d / hour | generic / 4MB | 1153.445 | 1216.825 | 1358.683 | 2597892 / 1298946 |
| 30d / day | generic / 4MB | 1153.029 | 1205.089 | 1277.387 | 2597892 / 1298946 |

JIT was enabled but not activated in these plans (EXPLAIN JIT absent).
Custom 30d/4MB temp blocks read/written: totals 726/729, series 938/941,
candidate 1244/1248. At custom/16MB all three have zero temp blocks.
Generic candidate still spills 1242/1243 at 16MB; no production plan-mode
change is proposed. All 24-hour cases have zero temp blocks. Buffer read
counts were zero after fixture seeding; these are warm measurements, not cold.
Baseline full HTTP single captures: 24h/hour 201.927ms, 24h/day 199.494ms,
30d/hour 592.476ms, 30d/day 566.441ms. HTTP and instrumented EXPLAIN timings
are not interchangeable.

Command: `LUX_TEST_PG='postgres://lux:lux@127.0.0.1:55432/postgres?sslmode=disable' LUX_MONTHLY_EXPERIMENT=1 go test ./internal/server -run '^TestCostMonthlyExperiment$' -v -count=1 -timeout 20m`.
Result before implementation: PASS, 129.321s, one top-level test, no skips.

## Scope

Implement only one family group with interval and no top/runs. The measured
case avoids materializing wide raw rows or changing ranking/distinct counts.
Other cases retain their original SQL and behavior. Family/label filters and
tenant RLS remain in the original shared query. Unallocated stays separate.
The explicit total discriminator separates totals from timestamps.

## Verification And Limitations

Production change is restricted to `internal/server/costread.go`; focused
tests and benchmark are in `internal/server/costmonthly_test.go`.
The original path remains for every request outside the narrow scope.
There are no new production helpers, caches, or settings.

Post-change full ordered SQL equality and HTTP equivalence passed. Actual
issued heavy SQL count fell from two to one. Sized-work guard observed
303756 buffers for totals alone and the same 303756 for combined totals and
series, and requires one heavy query (the old path fails that requirement).

Post-change HTTP results, milliseconds, three warm-data repetitions:

| Window / Bucket | First After Local PG Restart | Warm-Data Median |
| --- | ---: | ---: |
| 24h / hour | 142.127 | 116.896 |
| 24h / day | 138.071 | 115.147 |
| 30d / hour | 557.899 | 532.459 |
| 30d / day | 449.636 | 426.224 |

These HTTP requests use fresh pools, so data is warm but prepared statements
are not reused. Restart clears PostgreSQL shared buffers, not the kernel page
cache. Baseline HTTP captures were not repeated or restart-cold, so they are
not a controlled cold before/after comparison. The controlled before/after
evidence is the same-fixture typed SQL experiment captured before editing.
Post-change experiment: PASS, 139.034s, one test, no skips; all mode/memory/
window/bucket comparisons equal. It reuses unchanged totals and runs=true
series production paths as result oracles, so its post-change series plan
is not an exact reproduction of the earlier plain-series baseline plan.

Focused gate command:
`LUX_TEST_PG='postgres://lux:lux@127.0.0.1:55432/postgres?sslmode=disable' go test ./internal/server -run 'TestCost(Summary|MonthlyWork|MonthlyFamily|ByMember|Rank|HostRows|Read|Labels)|Test.*(KeyPrivacy|Cost.*Filter|RLS)' -v -count=1 -timeout 40m`.
PASS: 24 top-level tests, zero skips, 34.623s. This includes top/rank/ties,
cardinality, labels, ownership, key grouping, RLS, unallocated visibility,
Run names and concurrent replacement. An earlier attempt was invalidated by
mistakenly running it concurrently with the local restart benchmark; it failed
one test and skipped 18, and was discarded and rerun cleanly.
`go vet ./internal/server ./internal/store` passed.
Review found no production correctness issues; it identified fresh-pool HTTP
labeling and ineffective label test cases, both corrected. Final focused
`go test ./internal/server -run '^TestCostMonthlyFamily' -v -count=1 -timeout 5m`
passed both tests, no skips, 2.803s: matching/selective labels and absence
filters, empty results, family inclusion/exclusion, tenant/operator scope,
hour/day output equality, and exactly 10000/10001 combined-row limits.
Full suites and final verification pending.
512MB settings are deliberately not changed; the parent owns that branch.
No remote publication requested beyond launcher branch export; export details
will be included in the final response.
