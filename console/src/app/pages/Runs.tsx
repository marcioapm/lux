import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Button, Card, RUN_STATE_LIST, runStateStyle, Table } from "../../ds/index.ts";
import { api, useNow, useQuery, useSession } from "../../api/index.ts";
import { go, Link, setSearchParams, useSearchParams } from "../router.tsx";
import { useScope } from "../scope.tsx";
import { ErrorBlock, ErrorStrip, hostPath, runColumns, runPath } from "./common.tsx";

const PAGE = 100;
/** The API's largest page: past it, narrow the filters. */
const MAX = 1000;

/**
 * Runs list. Every filter lives in the URL (?state=a,b&resumable=true&host=&label=),
 * so "runs on host X" or "queued runs" are links. State chips select any of
 * those states; Resumable narrows to what resume accepts; all filters AND.
 */
export function Runs() {
  const scope = useScope();
  const session = useSession();
  const now = useNow();
  const params = useSearchParams();
  const states = useMemo(() => (params.get("state") ?? "").split(",").filter(Boolean), [params]);
  const resumable = params.get("resumable") === "true";
  const host = params.get("host") ?? "";
  const label = params.get("label") ?? "";

  const [hostDraft, setHostDraft] = useState(host);
  const [labelDraft, setLabelDraft] = useState(label);
  useEffect(() => setHostDraft(host), [host]);
  useEffect(() => setLabelDraft(label), [label]);

  // Pages loaded so far, for this filter only: a filter change starts over.
  const filterKey = `${scope.tenant}|${states.join(",")}|${resumable}|${host}|${label}`;
  const [paging, setPaging] = useState({ key: filterKey, pages: 1 });
  const pages = paging.key === filterKey ? paging.pages : 1;
  const limit = Math.min(MAX, PAGE * pages);

  // One list, refreshed as a whole: every page loaded so far.
  const q = useQuery(
    `runs:${filterKey}:${limit}`,
    async (s) => ({ limit, runs: await api.runs(scope.apiTenant, { state: states.length ? states : undefined, resumable: resumable || undefined, host: host || undefined, label: label || undefined, limit }, s) }),
    { interval: 5000, keep: pages > 1 },
  );
  const runs = q.data?.runs ?? [];
  const full = q.data != null && q.data.runs.length >= q.data.limit;
  const more = full && limit < MAX;
  const loadingMore = q.fetching && (q.data?.limit ?? 0) < limit;

  // Show a host id filter by name (a name filter is shown as typed).
  const hostInfo = useQuery(`host-name:${host}`, (s) => api.host(host, s), { enabled: host.startsWith("host_") });

  const toggleState = (s: string) => {
    const next = states.includes(s) ? states.filter((x) => x !== s) : [...states, s];
    setSearchParams({ state: next.length ? next.join(",") : null });
  };
  const submitText = (e: FormEvent) => {
    e.preventDefault();
    setSearchParams({ host: hostDraft.trim() || null, label: labelDraft.trim() || null });
  };
  const clear = () => setSearchParams({ state: null, resumable: null, host: null, label: null });
  const filtered = states.length > 0 || resumable || host !== "" || label !== "";

  const showTenant = session.role === "operator" && scope.apiTenant === undefined;
  const cols = useMemo(() => runColumns({ now, tenant: showTenant }), [showTenant, now]);

  return (
    <div className="page">
      <div className="filters">
        <div className="chips" role="group" aria-label="States">
          {RUN_STATE_LIST.map((s) => {
            const st = runStateStyle(s);
            const on = states.includes(s);
            return (
              <button key={s} type="button" className={["chip", `chip-${st.hue}`, on ? "is-on" : ""].join(" ").trim()} onClick={() => toggleState(s)} aria-pressed={on}>
                {st.label}
              </button>
            );
          })}
        </div>
        <label className="check" title="Stopped, lost or failed: what resume accepts">
          <input type="checkbox" checked={resumable} onChange={(e) => setSearchParams({ resumable: e.target.checked ? "true" : null })} />
          Resumable
        </label>
      </div>
      <form className="filters" onSubmit={submitText}>
        <input className="input input-sm mono" placeholder="host id or name" value={hostDraft} onChange={(e) => setHostDraft(e.target.value)} style={{ width: 190 }} />
        <input className="input input-sm mono" placeholder="label k=v" value={labelDraft} onChange={(e) => setLabelDraft(e.target.value)} style={{ width: 150 }} />
        <Button size="sm" type="submit">
          Apply
        </Button>
        {filtered && (
          <Button size="sm" variant="ghost" onClick={clear}>
            Clear
          </Button>
        )}
        <span className="filter-summary">
          Showing {filtered ? "runs" : "all runs"}
          {states.length > 0 && (
            <>
              {" "}in state <span className="mono">{states.join(" or ")}</span>
            </>
          )}
          {resumable && <>{states.length > 0 ? " and" : ""} resumable (stopped, lost or failed)</>}
          {host && (
            <>
              {" "}placed on{" "}
              {hostInfo.data ? (
                <Link to={hostPath(hostInfo.data.id)} className="mono">
                  {hostInfo.data.name}
                </Link>
              ) : (
                <span className="mono">{host}</span>
              )}
            </>
          )}
          {label && (
            <>
              {" "}labelled <span className="mono">{label}</span>
            </>
          )}
          .
        </span>
      </form>
      <Card flush title="Runs" subtitle={`${runs.length}${full ? "+" : ""} · newest first · refreshes every 5s`}>
        <ErrorStrip error={runs.length > 0 ? q.error : null} />
        {q.error && runs.length === 0 && !q.loading ? (
          <ErrorBlock error={q.error} onRetry={q.refetch} />
        ) : (
          <Table columns={cols} rows={runs} rowKey={(r) => r.id} loading={q.loading} onRowClick={(r) => go(runPath(r.id))} empty="No runs match these filters." />
        )}
        {(more || (full && limit >= MAX)) && (
          <div className="table-foot">
            {more ? (
              <Button size="sm" onClick={() => setPaging({ key: filterKey, pages: pages + 1 })} loading={loadingMore}>
                Load more
              </Button>
            ) : (
              <span className="muted">Showing the newest {MAX}. Narrow the filters to see older runs.</span>
            )}
          </div>
        )}
      </Card>
    </div>
  );
}
