package server

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

type costSummaryInput struct {
	CostLabelQuery
	Group    []string `query:"group,explode" doc:"Repeat up to twice: tenant (operators), pool, host, family, run, key, label:key."`
	Family   string   `query:"family" doc:"Only this family."`
	NoFamily string   `query:"nofamily" doc:"Every family but this one."`
	Interval string   `query:"interval" doc:"hour or day; include a time series."`
	Top      string   `query:"top" doc:"1 to 50: keep the first group's N values costing the most per currency (ties by value) and fold the rest into the reserved value (other), in totals and series; (none) is never folded nor counted, and the second group is kept. Needs a group. The row limit applies to the folded rows. Totals rows then carry runs, and otherCount says how many values were folded."`
	Rank     string   `query:"rank" doc:"With top: all (default), compute, or external (every family but compute): the cost that ranks the values and that runs counts."`
}

func (in *costSummaryInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	in.Group = u.Query()["group"]
	in.CostLabelQuery.resolve(u.Query())
	return nil
}

// CostLabelQuery filters cost to Runs by their labels. Exported: huma
// binds only the parameters of exported embedded structs.
type CostLabelQuery struct {
	HistoryQuery
	Label   []string `query:"label,explode" doc:"Only Runs with this label (key=value, split at the first =). Repeat: the same key repeated matches any of its values; different keys must all match."`
	NoLabel []string `query:"nolabel,explode" doc:"Only Runs without this label key. Repeatable."`
}

func (q *CostLabelQuery) resolve(v url.Values) {
	q.Label, q.NoLabel = v["label"], v["nolabel"]
}

const costMaxLabelFilters = 64

// labelFilter is the label and nolabel filters as the scoped CTE's bind
// parameters: {key: [values]} and the keys that must be absent.
func (q *CostLabelQuery) labelFilter() (map[string][]string, []string, error) {
	if len(q.Label)+len(q.NoLabel) > costMaxLabelFilters {
		return nil, nil, errf(http.StatusBadRequest, "bad_request", "at most %d label filters", costMaxLabelFilters)
	}
	want := map[string][]string{}
	for _, l := range q.Label {
		k, v, ok := strings.Cut(l, "=")
		if !ok || !labelKeyRe.MatchString(k) {
			return nil, nil, errf(http.StatusBadRequest, "bad_request", "label %q: want key=value with a valid label key", l)
		}
		if !slices.Contains(want[k], v) {
			want[k] = append(want[k], v)
		}
	}
	absent := []string{}
	for _, k := range q.NoLabel {
		if !labelKeyRe.MatchString(k) {
			return nil, nil, errf(http.StatusBadRequest, "bad_request", "nolabel %q: not a valid label key", k)
		}
		absent = append(absent, k)
	}
	return want, absent, nil
}

// costScopedSQL is the costed Run hours in [$1, $2) of family $3 (or all),
// whose Run's labels match $4 ({key: [values]}: one of the values of every
// key) and lack every key in $5. Filters are values, never SQL text.
const costScopedSQL = `scoped AS (
		SELECT c.hour, c.currency, c.amount, c.tenant_id, c.family, c.run_id,
			coalesce(cp.name, '(none)') AS pool, coalesce(c.host_id, '(none)') AS host, r.labels,
			coalesce(r.submitted_by_key, 'email:' || r.submitted_by_email, '(none)') AS submitter
		FROM cost_hourly c JOIN runs r ON r.id = c.run_id LEFT JOIN pools cp ON cp.id = c.pool_id
		WHERE c.run_id IS NOT NULL AND c.hour >= $1 AND c.hour < $2
			AND ($3 = '' OR c.family = $3)
			AND NOT EXISTS (SELECT 1 FROM jsonb_each($4::jsonb) f
				WHERE (r.labels->>f.key) IN (SELECT jsonb_array_elements_text(f.value)) IS NOT TRUE)
			AND NOT EXISTS (SELECT 1 FROM unnest($5::text[]) k WHERE r.labels ? k)
	)`

// costTx runs read in a read-only snapshot under the principal's RLS scope.
func (s *Server) costTx(ctx context.Context, p Principal, read func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.db.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if p.TenantID == "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `SELECT set_config('lux.tenant_id', $1, true)`, p.TenantID); err != nil {
			return err
		}
		return read(tx)
	})
}

type CostSummaryRow struct {
	At       *time.Time        `json:"at,omitempty"`
	Group    map[string]string `json:"group,omitempty"`
	Currency string            `json:"currency"`
	Amount   string            `json:"amount"`
	Runs     *int              `json:"runs,omitempty" doc:"With top, on totals: the Runs with cost that rank counts under this row's first-group value, across the second group."`
}

type HostAllocation struct {
	HostID      string `json:"hostId"`
	Currency    string `json:"currency"`
	Allocated   string `json:"allocated"`
	Unallocated string `json:"unallocated"`
}

type CostSummaryBody struct {
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`
	Basis       string           `json:"basis"`
	Totals      []CostSummaryRow `json:"totals"`
	Series      []CostSummaryRow `json:"series,omitempty"`
	Unallocated []CostSummaryRow `json:"unallocated,omitempty"`
	Hosts       []HostAllocation `json:"hosts,omitempty"`
	Families    []CostFamilyInfo `json:"families,omitempty" doc:"Grouped by family: each family in totals, with the displayName and color byFamily has on a Run's cost."`
	Runs        []CostRunInfo    `json:"runs,omitempty" doc:"Grouped by run: each Run in totals with its name and labels."`
	Keys        []CostKeyInfo    `json:"keys,omitempty" doc:"Grouped by key: each submitter in totals. (none) is Runs from before luxd recorded who submitted them."`
	OtherCount  map[string]int   `json:"otherCount,omitempty" doc:"With top: per currency, how many values (other) holds; a currency with none folded is absent."`
}

type CostKeyInfo struct {
	ID       string `json:"id" doc:"The group value: an API key id, or email:<address> for a person."`
	Name     string `json:"name,omitempty" doc:"The key's name; absent for an operator's key when a tenant asks."`
	Operator bool   `json:"operator,omitempty" doc:"An operator's key."`
	Revoked  bool   `json:"revoked,omitempty"`
	Email    string `json:"email,omitempty" doc:"A person's address."`
}

type CostRunInfo struct {
	ID     string            `json:"id"`
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

type CostFamilyInfo struct {
	Family      string `json:"family"`
	DisplayName string `json:"displayName,omitempty"`
	Color       string `json:"color,omitempty"`
}

type costSummaryOutput struct {
	Body CostSummaryBody `nameHint:"CostSummary"`
}

const (
	costMaxRange = 90 * 24 * time.Hour
	costMaxRows  = 10000
	costMaxTop   = 50
	// costOther is the value a top fold gives every value past the top N.
	costOther = "(other)"
)

// fold is top (0: none) and what it ranks by.
func (in *costSummaryInput) fold() (int, string, error) {
	rank := in.Rank
	if rank == "" {
		rank = "all"
	}
	if rank != "all" && rank != "compute" && rank != "external" {
		return 0, "", errf(http.StatusBadRequest, "bad_request", "rank must be all, compute or external")
	}
	if in.Top == "" {
		if in.Rank != "" {
			return 0, "", errf(http.StatusBadRequest, "bad_request", "rank needs top")
		}
		return 0, rank, nil
	}
	top, err := strconv.Atoi(in.Top)
	if err != nil || top < 1 || top > costMaxTop {
		return 0, "", errf(http.StatusBadRequest, "bad_request", "top must be 1 to %d", costMaxTop)
	}
	if len(in.Group) == 0 {
		return 0, "", errf(http.StatusBadRequest, "bad_request", "top needs a group")
	}
	return top, rank, nil
}

// Cost buckets are whole UTC hours; a partial boundary includes its hour.
func costRange(from, to time.Time) (time.Time, time.Time, error) {
	if !from.Before(to) {
		return from, to, errf(http.StatusBadRequest, "bad_request", "from must precede to")
	}
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC()
	if !to.Equal(to.Truncate(time.Hour)) {
		to = to.Truncate(time.Hour).Add(time.Hour)
	}
	if to.Sub(from) > costMaxRange {
		return from, to, errf(http.StatusBadRequest, "bad_request", "cost range must not exceed 90 days")
	}
	return from, to, nil
}

func costRowLimit(n int) error {
	if n > costMaxRows {
		return errf(http.StatusRequestEntityTooLarge, "too_large", "cost response exceeds %d rows", costMaxRows)
	}
	return nil
}

func (s *Server) costSummary(ctx context.Context, in *costSummaryInput) (*costSummaryOutput, error) {
	p := principal(ctx)
	from, to, _, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	from, to, err = costRange(from, to)
	if err != nil {
		return nil, err
	}
	if in.Interval != "" && in.Interval != "hour" && in.Interval != "day" {
		return nil, errf(http.StatusBadRequest, "bad_request", "interval must be hour or day")
	}
	if len(in.Group) > 2 {
		return nil, errf(http.StatusBadRequest, "bad_request", "at most two groups")
	}
	var group [2]string
	var label [2]string
	for i, g := range in.Group {
		switch {
		case g == "tenant" && p.Operator, g == "pool", g == "host", g == "family", g == "run", g == "key":
			group[i] = g
		case strings.HasPrefix(g, "label:") && len(g) > len("label:"):
			group[i], label[i] = "label", strings.TrimPrefix(g, "label:")
		default:
			return nil, errf(http.StatusBadRequest, "bad_request", "invalid group %q", g)
		}
		if i > 0 && g == in.Group[0] {
			return nil, errf(http.StatusBadRequest, "bad_request", "duplicate group %q", g)
		}
	}
	want, absent, err := in.labelFilter()
	if err != nil {
		return nil, err
	}
	top, rank, err := in.fold()
	if err != nil {
		return nil, err
	}
	filtered := len(want) > 0 || len(absent) > 0
	out := &costSummaryOutput{Body: CostSummaryBody{From: from, To: to, Basis: "list", Totals: []CostSummaryRow{}}}
	// Group selectors and label keys are values, never SQL identifiers.
	const base = `WITH ` + costScopedSQL + `, dimensions AS (
		SELECT hour, currency, amount, run_id, ($10 = 'all' OR (family = 'compute') = ($10 = 'compute')) AS ranks,
			CASE $6::text WHEN 'tenant' THEN tenant_id WHEN 'pool' THEN pool WHEN 'host' THEN host WHEN 'key' THEN submitter
				WHEN 'family' THEN family WHEN 'run' THEN run_id WHEN 'label' THEN coalesce(labels->>$8, '(none)') END AS g1,
			CASE $7::text WHEN 'tenant' THEN tenant_id WHEN 'pool' THEN pool WHEN 'host' THEN host WHEN 'key' THEN submitter
				WHEN 'family' THEN family WHEN 'run' THEN run_id WHEN 'label' THEN coalesce(labels->>$9, '(none)') END AS g2
		FROM scoped WHERE $12 = '' OR family <> $12
	)`
	// The fold ranks g1 per currency by the cost rank counts (a value with
	// none ranks last), then names every value past $11 (other). nvals is
	// every value but (none): the folded ones are nvals - $11. vals is read
	// by the totals only: Runs per folded value, across g2.
	const folded = `, sums AS (
		SELECT g1, currency, sum(amount) FILTER (WHERE ranks) AS total FROM dimensions WHERE g1 <> '(none)' GROUP BY 1, 2
	), ranked AS (
		SELECT g1, currency, row_number() OVER (PARTITION BY currency ORDER BY total DESC NULLS LAST, g1 COLLATE "C") AS n,
			count(*) OVER (PARTITION BY currency) AS nvals
		FROM sums
	), shaped AS (
		SELECT d.hour, d.currency, d.amount, d.run_id, d.ranks, d.g2, rk.nvals,
			CASE WHEN rk.n > $11 THEN '(other)' ELSE d.g1 END AS g1
		FROM dimensions d LEFT JOIN ranked rk ON rk.g1 = d.g1 AND rk.currency = d.currency
	), vals AS (
		SELECT g1, currency, (count(DISTINCT run_id) FILTER (WHERE ranks))::int AS runs, max(nvals)::int AS nvals
		FROM shaped GROUP BY 1, 2
	) `
	const plain = `, shaped AS (SELECT * FROM dimensions WHERE $11::int = 0) `
	query := base + plain
	if top > 0 {
		query = base + folded
	}
	args := []any{from, to, in.Family, want, absent, group[0], group[1], label[0], label[1], rank, top, in.NoFamily}
	rowCount := 0
	read := func(tx pgx.Tx, interval string) ([]CostSummaryRow, error) {
		bucket := "NULL::timestamptz"
		switch interval {
		case "hour":
			bucket = "date_trunc('hour', hour)"
		case "day":
			bucket = "date_trunc('day', hour AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"
		}
		grouped := `SELECT ` + bucket + ` AS at, g1, g2, currency, trim_scale(sum(amount))::text AS amount FROM shaped GROUP BY 1, 2, 3, 4`
		var sql string
		if top > 0 && interval == "" {
			sql = query + `SELECT b.*, v.runs, v.nvals FROM (` + grouped + `) b
				LEFT JOIN vals v ON v.g1 IS NOT DISTINCT FROM b.g1 AND v.currency = b.currency ORDER BY 1 NULLS FIRST, 2, 3, 4`
		} else {
			sql = query + `SELECT *, NULL::int, NULL::int FROM (` + grouped + `) b ORDER BY 1 NULLS FIRST, 2, 3, 4`
		}
		result := []CostSummaryRow{}
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var row CostSummaryRow
			var g1, g2 *string
			var nvals *int
			if err := rows.Scan(&row.At, &g1, &g2, &row.Currency, &row.Amount, &row.Runs, &nvals); err != nil {
				return nil, err
			}
			if g1 != nil && *g1 == costOther && nvals != nil && *nvals > top {
				if out.Body.OtherCount == nil {
					out.Body.OtherCount = map[string]int{}
				}
				out.Body.OtherCount[row.Currency] = *nvals - top
			}
			if len(in.Group) > 0 {
				value := "(none)"
				if g1 != nil {
					value = *g1
				}
				row.Group = map[string]string{in.Group[0]: value}
			}
			if len(in.Group) > 1 {
				value := "(none)"
				if g2 != nil {
					value = *g2
				}
				row.Group[in.Group[1]] = value
			}
			result = append(result, row)
			rowCount++
			if err := costRowLimit(rowCount); err != nil {
				return nil, err
			}
		}
		return result, rows.Err()
	}
	err = s.costTx(ctx, p, func(tx pgx.Tx) error {
		var err error
		out.Body.Totals, err = read(tx, "")
		if err != nil {
			return err
		}
		if in.Interval != "" {
			out.Body.Series, err = read(tx, in.Interval)
			if err != nil {
				return err
			}
		}
		if group[0] == "run" || group[1] == "run" {
			if out.Body.Runs, err = summaryRuns(ctx, tx, out.Body.Totals); err != nil {
				return err
			}
		}
		// Unallocated cost belongs to no Run, so it has no labels to filter.
		if p.Operator && p.TenantID == "" && !filtered {
			out.Body.Unallocated = []CostSummaryRow{}
			rows, err := tx.Query(ctx, `SELECT currency, trim_scale(sum(unallocated))::text FROM cost_hourly
				WHERE run_id IS NULL AND hour >= $1 AND hour < $2 AND ($3 = '' OR family = $3) AND ($4 = '' OR family <> $4)
				GROUP BY currency ORDER BY currency`, from, to, in.Family, in.NoFamily)
			if err != nil {
				return err
			}
			for rows.Next() {
				var row CostSummaryRow
				if err := rows.Scan(&row.Currency, &row.Amount); err != nil {
					rows.Close()
					return err
				}
				out.Body.Unallocated = append(out.Body.Unallocated, row)
				rowCount++
				if err := costRowLimit(rowCount); err != nil {
					rows.Close()
					return err
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil || group[0] != "host" && group[1] != "host" {
				return err
			}
			out.Body.Hosts = []HostAllocation{}
			rows, err = tx.Query(ctx, `SELECT host_id, currency, trim_scale(sum(allocated))::text,
				trim_scale(sum(unallocated))::text FROM cost_hourly
				WHERE run_id IS NULL AND hour >= $1 AND hour < $2 AND ($3 = '' OR family = $3) AND ($4 = '' OR family <> $4)
				GROUP BY host_id, currency ORDER BY host_id, currency`, from, to, in.Family, in.NoFamily)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var row HostAllocation
				if err := rows.Scan(&row.HostID, &row.Currency, &row.Allocated, &row.Unallocated); err != nil {
					return err
				}
				out.Body.Hosts = append(out.Body.Hosts, row)
				rowCount++
				if err := costRowLimit(rowCount); err != nil {
					return err
				}
			}
			return rows.Err()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if group[0] == "family" || group[1] == "family" {
		out.Body.Families = s.summaryFamilies(out.Body.Totals)
	}
	if group[0] == "key" || group[1] == "key" {
		if out.Body.Keys, err = s.summaryKeys(ctx, p, out.Body.Totals); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// summaryKeys names each submitter in rows. The ids come from Runs the
// caller sees; keys are read as the system and named by keyName.
func (s *Server) summaryKeys(ctx context.Context, p Principal, rows []CostSummaryRow) ([]CostKeyInfo, error) {
	seen := map[string]bool{}
	for _, r := range rows {
		if v := r.Group["key"]; v != "(none)" && v != costOther {
			seen[v] = true
		}
	}
	all := slices.Sorted(maps.Keys(seen))
	out := make([]CostKeyInfo, 0, len(all))
	var keyIDs []string
	for _, v := range all {
		if email, ok := strings.CutPrefix(v, "email:"); ok {
			out = append(out, CostKeyInfo{ID: v, Email: email})
		} else {
			keyIDs = append(keyIDs, v)
		}
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, name, tenant_id, revoked_at IS NOT NULL
			FROM api_keys WHERE id = ANY($1) ORDER BY id`, keyIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k CostKeyInfo
			var name string
			var keyTenant *string
			var revoked bool
			if err := rows.Scan(&k.ID, &name, &keyTenant, &revoked); err != nil {
				return err
			}
			k.Operator = keyTenant == nil
			k.Name, k.Revoked = keyName(p, keyTenant, name, revoked)
			out = append(out, k)
		}
		return rows.Err()
	})
	slices.SortFunc(out, func(a, b CostKeyInfo) int { return strings.Compare(a.ID, b.ID) })
	return out, err
}

type costLabelsInput struct {
	CostLabelQuery
}

func (in *costLabelsInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	in.CostLabelQuery.resolve(u.Query())
	return nil
}

type CostLabelKey struct {
	Key  string `json:"key"`
	Runs int    `json:"runs" doc:"Runs with cost in the range that carry this label."`
}

type costLabelsOutput struct {
	Body struct {
		From time.Time      `json:"from"`
		To   time.Time      `json:"to"`
		Keys []CostLabelKey `json:"keys" doc:"Most Runs first, then by key."`
	} `nameHint:"CostLabels"`
}

// costLabels lists the label keys on Runs with cost in the range (after
// the label filters), for picking a breakdown or a filter; the values and
// their cost are GET /v1/costs?group=label:<key>.
func (s *Server) costLabels(ctx context.Context, in *costLabelsInput) (*costLabelsOutput, error) {
	p := principal(ctx)
	from, to, _, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	if from, to, err = costRange(from, to); err != nil {
		return nil, err
	}
	want, absent, err := in.labelFilter()
	if err != nil {
		return nil, err
	}
	out := &costLabelsOutput{}
	out.Body.From, out.Body.To, out.Body.Keys = from, to, []CostLabelKey{}
	err = s.costTx(ctx, p, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `WITH `+costScopedSQL+`
			SELECT k, count(DISTINCT run_id)::int FROM scoped, jsonb_object_keys(labels) k
			GROUP BY k ORDER BY 2 DESC, 1`, from, to, "", want, absent)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k CostLabelKey
			if err := rows.Scan(&k.Key, &k.Runs); err != nil {
				return err
			}
			out.Body.Keys = append(out.Body.Keys, k)
			if err := costRowLimit(len(out.Body.Keys)); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// summaryFamilies names each family in rows once, sorted, as byFamily does.
func (s *Server) summaryFamilies(rows []CostSummaryRow) []CostFamilyInfo {
	seen := map[string]bool{}
	for _, r := range rows {
		if f := r.Group["family"]; f != costOther {
			seen[f] = true
		}
	}
	names := slices.Sorted(maps.Keys(seen))
	meta := s.costFamilyMetadata()
	out := make([]CostFamilyInfo, 0, len(names))
	for _, family := range names {
		name, color := meta.lookup(family)
		out = append(out, CostFamilyInfo{Family: family, DisplayName: name, Color: color})
	}
	return out
}

// summaryRuns names each Run in rows, in one query in the summary's snapshot.
func summaryRuns(ctx context.Context, tx pgx.Tx, rows []CostSummaryRow) ([]CostRunInfo, error) {
	seen := map[string]bool{}
	for _, r := range rows {
		if id := r.Group["run"]; id != costOther {
			seen[id] = true
		}
	}
	ids := slices.Sorted(maps.Keys(seen))
	names, err := tx.Query(ctx, `SELECT id, name, nullif(labels, '{}') FROM runs WHERE id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(names, pgx.RowToStructByPos[CostRunInfo])
}

type hostCostInput struct {
	HostPath
	HistoryQuery
}
type hostCostHour struct {
	Hour        time.Time `json:"hour"`
	Currency    string    `json:"currency"`
	Allocated   string    `json:"allocated"`
	Unallocated *string   `json:"unallocated,omitempty"`
}
type hostCostRate struct {
	From     time.Time  `json:"from"`
	To       *time.Time `json:"to,omitempty"`
	PerHour  string     `json:"perHour"`
	Currency string     `json:"currency"`
	Source   string     `json:"source"`
}
type hostCostOutput struct {
	Body struct {
		HostID string         `json:"hostId"`
		From   time.Time      `json:"from"`
		To     time.Time      `json:"to"`
		Basis  string         `json:"basis"`
		Hours  []hostCostHour `json:"hours"`
		Rates  []hostCostRate `json:"rates,omitempty"`
	} `nameHint:"HostCost"`
}

func (s *Server) hostCost(ctx context.Context, in *hostCostInput) (*hostCostOutput, error) {
	p := principal(ctx)
	from, to, _, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	from, to, err = costRange(from, to)
	if err != nil {
		return nil, err
	}
	out := &hostCostOutput{}
	out.Body.From, out.Body.To, out.Body.Basis = from, to, "list"
	out.Body.Hours = []hostCostHour{}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		id, err := s.resolveHost(ctx, tx, p, in.ID, true)
		if err != nil {
			return err
		}
		if p.TenantID != "" {
			var own bool
			if err := tx.QueryRow(ctx, `SELECT tenant_id IS NOT DISTINCT FROM $2 FROM hosts WHERE id = $1`, id, p.TenantID).Scan(&own); err != nil {
				return err
			}
			if !own {
				return errf(http.StatusForbidden, "forbidden", "a host's cost is its owner's or the operators'")
			}
		}
		out.Body.HostID = id
		rows, err := tx.Query(ctx, `SELECT hour, currency, trim_scale(sum(allocated))::text,
			trim_scale(sum(unallocated))::text FROM cost_hourly
			WHERE host_id = $1 AND run_id IS NULL AND hour >= $2 AND hour < $3
			GROUP BY hour, currency ORDER BY hour, currency`, id, from, to)
		if err != nil {
			return err
		}
		for rows.Next() {
			var h hostCostHour
			if err := rows.Scan(&h.Hour, &h.Currency, &h.Allocated, &h.Unallocated); err != nil {
				rows.Close()
				return err
			}
			if p.TenantID != "" {
				h.Unallocated = nil
			}
			out.Body.Hours = append(out.Body.Hours, h)
			if err := costRowLimit(len(out.Body.Hours)); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		out.Body.Rates = []hostCostRate{}
		if p.TenantID != "" {
			return nil
		}
		rows, err = tx.Query(ctx, `SELECT valid_from, valid_to, trim_scale(per_hour)::text, currency, source
			FROM host_rates WHERE host_id = $1 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $2)
			ORDER BY valid_from`, id, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r hostCostRate
			if err := rows.Scan(&r.From, &r.To, &r.PerHour, &r.Currency, &r.Source); err != nil {
				return err
			}
			out.Body.Rates = append(out.Body.Rates, r)
			if err := costRowLimit(len(out.Body.Hours) + len(out.Body.Rates)); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
