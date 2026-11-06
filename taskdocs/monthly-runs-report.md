# Monthly Cost Runs-Enabled Single Pass

Commit: `42088433` (`Read family cost totals and series with Run counts in one pass`).
Base: `ed558707` (`Read plain family cost totals and series in one pass`).

## Scope

The single-pass path now covers the actual Overview request shape:
`group=family`, an hour/day `interval`, `top=0`, and `runs=true`. The shared
scoped query and CTEs are used once. `vals` is materialized into the existing
JSON key map. Grouping sets emit bucket rows and total rows, with
`grouping(bucket)` as the total discriminator. Run and nvals metadata is read
from the map only for total rows; series metadata remains NULL. The plain
family path remains equivalent and still uses the same grouping sets without
the map lookup.

No API, cache, run-owner, RLS, force-custom-plan, frontend, production, AWS,
deployment, or PostgreSQL configuration changes were made.

## Fixture And Environment

- Fixture: 216,000 hourly cost rows, 27,000 Runs, 30 days, two tenants, two pools, eight families, two currencies, and labels.
- Window: `2026-08-27T10:00:00Z` through `2026-09-26T10:00:00Z` for the sized benchmark.
- Database: PostgreSQL 18.6 Alpine, linux/arm64, stock `shared_buffers=128MB`, `work_mem=4MB`, `jit=on`.
- Database role check: `current_user=lux_app`; neither superuser nor `rolbypassrls`.
- The current runs-enabled HTTP SQL and typed binds were captured before editing. The frozen old-path oracle in the focused test is based on that capture, not on reading production source after the edit.

## Correctness Tests

`TestCostMonthlyFamilyEquivalence` makes real HTTP calls with `runs=true` and
checks the complete response against the captured old totals and series SQL.
It covers hour and day intervals, tenant and operator principals, operator
tenant narrowing, multicurrency, a Run crossing UTC days, count-distinct
semantics, labels and no-labels, matching and nonmatching family filters,
unallocated/empty filtered results, and zero-valued rows. It also asserts one
captured heavy statement and that counts exist only on totals.

`TestCostMonthlyFamilyRowLimit` covers both plain and `runs=true` requests at
10,000 rows and one over the limit. `TestCostMonthlyWork` checks that the
combined path has one scoped statement and does not repeat buffer work.

Focused command and result:

```text
LUX_TEST_PG='postgres://lux:lux@127.0.0.1:55432/postgres?sslmode=disable' \
go test ./internal/server -run 'TestCost(Summary|MonthlyWork|MonthlyFamily|ByMember|Rank|HostRows|Read|Labels)|Test.*(KeyPrivacy|Cost.*Filter|RLS)' -v -count=1 -timeout 40m
PASS: internal/server, 24 top-level tests, 34.989s, zero skips
```

The full real-PostgreSQL server and store suites also passed:

```text
go test ./internal/server ./internal/store -v -count=1 -timeout 40m
PASS: internal/server, 1171.556s
PASS: internal/store, 19.843s
```

The count-corruption mutant (`total runs + 1`) failed the full-response
equivalence test. The two-query mutant (`!in.Runs` restored in the single-pass
guard) failed the one-heavy-statement work test. Both mutants were removed
before the commit.

## Performance

`TestCostMonthlyExperiment` reused the 216k-row fixture and ran three warm
`EXPLAIN (ANALYZE, BUFFERS)` attempts for each query under custom and generic
plan modes. Query 0 is totals, query 1 is series, and query 2 is the actual
runs-enabled candidate. Values below are execution-time medians in ms. Buffer
counts are shared hit plus read blocks; this fixture was warm and had zero
shared reads.

| Window / interval | Mode / memory | Totals | Series | Candidate | Candidate buffers | Candidate temp read/write |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| 24h / hour | custom / 4MB | 97.298 | 93.309 | 99.935 | 94,956 | 0 / 0 |
| 24h / day | custom / 4MB | 97.727 | 94.623 | 99.116 | 94,956 | 0 / 0 |
| 24h / hour | generic / 4MB | 49.956 | 45.913 | 52.492 | 45,882 | 0 / 0 |
| 24h / day | generic / 4MB | 50.100 | 46.218 | 52.440 | 45,882 | 0 / 0 |
| 30d / hour | custom / 4MB | 490.417 | 356.195 | 566.494 | 303,756 | 2,239 / 2,974 |
| 30d / day | custom / 4MB | 487.855 | 356.134 | 554.090 | 303,756 | 2,239 / 2,974 |
| 30d / hour | generic / 4MB | 1,395.275 | 1,264.230 | 1,479.041 | 1,298,946 | 2,214 / 2,947 |
| 30d / day | generic / 4MB | 1,392.483 | 1,258.327 | 1,477.573 | 1,298,946 | 2,214 / 2,947 |

The candidate and totals-only request both used 303,756 shared blocks for the
30-day custom-plan case, while the old two-query handler would perform both
scoped passes. JIT was enabled but not activated (`JIT=null` in the plans).

Fresh-pool HTTP medians for the actual runs-enabled request were 117.778ms
(24h/hour), 118.495ms (24h/day), 532.495ms (30d/hour), and 503.034ms
(30d/day). These are warm-data measurements and are not cold before/after
comparisons.

## Checks And Limitations

- `go vet ./internal/server ./internal/store` passed.
- `gofmt` and `git diff --check` passed.
- No production review lenses or simplification review were claimed here.
- The benchmark used 4MB work memory only; the parent-owned 512MB branch was not changed.
- Generic-plan timings are diagnostic; the implementation retains the existing forced custom plan behavior.
- The experiment covers family grouping with interval and runs metadata, not every possible cost grouping with `runs=true`; those paths retain their existing SQL.
- No production or AWS measurements were performed.
