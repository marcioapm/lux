// Small pieces shared by pages: error/loading blocks, links, the runs table
// columns, chart series builders and lookups.
import type { ReactNode } from "react";
import { Badge, Button, EmptyState, formatCores, formatBytes, formatRelative, formatTimestamp, IdChip, KeyValue, Skeleton, SkeletonLines, StatePill, Tooltip, type Column, type Unit } from "../../ds/index.ts";
import type { Run, Sample } from "../../api/index.ts";
import { Link, linkTo, useSearch } from "../router.tsx";

export function ErrorBlock({ error, onRetry, compact }: { error: string; onRetry?: () => void; compact?: boolean }) {
  return (
    <EmptyState
      compact={compact}
      title={error.startsWith("Your key cannot") ? "Not allowed" : "Request failed"}
      description={error}
      action={onRetry ? <Button size="sm" onClick={onRetry}>Retry</Button> : undefined}
    />
  );
}

/** Inline error strip under a card title (data is stale but still shown). */
export function ErrorStrip({ error }: { error: string | null }) {
  if (!error) return null;
  return (
    <div className="error-strip" role="alert">
      {error}
    </div>
  );
}

/** Placeholder for a page whose main object is still loading. */
export function PageSkeleton() {
  return (
    <div className="page" aria-busy="true">
      <div className="stack" style={{ gap: 8 }}>
        <Skeleton width={320} height={24} />
        <Skeleton width={480} height={14} />
      </div>
      <KeyValue columns={3} items={Array.from({ length: 6 }, () => ({ key: <Skeleton width={80} />, value: <Skeleton width="70%" /> }))} />
      <SkeletonLines lines={6} />
    </div>
  );
}

export const runPath = (id: string) => `/runs/${encodeURIComponent(id)}`;
export const hostPath = (id: string) => `/hosts/${encodeURIComponent(id)}`;
/** The runs list filtered to runs placed (any epoch) on a host. */
export const hostRunsPath = (hostId: string) => `/runs?host=${encodeURIComponent(hostId)}`;

/** Copyable id chip that links to `to` (client-side, keeping the scope). */
export function IdLink({ value, to, truncate, prefix }: { value: string; to: string; truncate?: number; prefix?: string }) {
  const l = linkTo(to, useSearch());
  return <IdChip value={value} truncate={truncate} prefix={prefix} href={l.href} onLinkClick={l.onClick} />;
}

export function RunLink({ id, truncate = 16 }: { id: string; truncate?: number }) {
  return <IdLink value={id} to={runPath(id)} truncate={truncate} />;
}

/** A host by name, linked by id (names can be reused). */
export function HostLink({ id, name }: { id: string; name?: string }) {
  return (
    <Link to={hostPath(id)} className="mono" title={id}>
      {name || id}
    </Link>
  );
}

/** State pill with the reason (why it waits, why it ended) dimmed below. */
export function StateCell({ kind, state, activity, reason, children }: { kind: "run" | "host"; state: string; activity?: string; reason?: string; children?: ReactNode }) {
  return (
    <span className="state-cell">
      <span className="row" style={{ gap: 6 }}>
        <StatePill kind={kind} state={state} activity={activity} />
        {children}
      </span>
      {reason && (
        <span className="state-reason" title={reason}>
          {reason}
        </span>
      )}
    </span>
  );
}

/** Shared columns of a runs table. */
export function runColumns({ now, tenant, host = true, adapter = true }: { now: number; tenant: boolean; host?: boolean; adapter?: boolean }): Column<Run>[] {
  const c: Column<Run>[] = [];
  if (tenant) c.push({ key: "tenant", header: "Tenant", cell: (r) => r.tenant, sortValue: (r) => r.tenant, width: 110, nowrap: true });
  c.push(
    { key: "id", header: "Run", cell: (r) => <RunLink id={r.id} />, sortValue: (r) => r.id, mono: true, width: 200 },
    { key: "name", header: "Name", cell: (r) => r.name || DASH, sortValue: (r) => r.name, nowrap: true },
    { key: "state", header: "State", cell: (r) => <StateCell kind="run" state={r.state} activity={r.activity} reason={r.stateReason} />, sortValue: (r) => r.state, width: 200 },
  );
  if (host) c.push({ key: "host", header: "Host", cell: (r) => (r.hostId ? <HostLink id={r.hostId} name={r.host} /> : DASH), sortValue: (r) => r.host, mono: true, width: 180, nowrap: true });
  if (adapter) c.push({ key: "adapter", header: "Adapter", cell: (r) => <Badge mono outline>{r.spec.workload.adapter}</Badge>, sortValue: (r) => r.spec.workload.adapter, width: 100 });
  c.push(
    { key: "epoch", header: "Epoch", cell: (r) => r.epoch, sortValue: (r) => r.epoch, align: "right", mono: true, width: 64 },
    { key: "created", header: "Created", cell: (r) => <Tooltip content={formatTimestamp(r.createdAt)}><span>{formatRelative(r.createdAt, now)}</span></Tooltip>, sortValue: (r) => Date.parse(r.createdAt), align: "right", width: 96 },
  );
  return c;
}

export interface SeriesData {
  x: number[];
  ys: (number | null)[][];
}

/** Pull aligned series out of history samples. Missing values stay null. */
export function seriesFrom(samples: Sample[] | undefined, pick: ((s: Sample) => number | null | undefined)[]): SeriesData {
  const x: number[] = [];
  const ys: (number | null)[][] = pick.map(() => []);
  for (const s of samples ?? []) {
    const t = Date.parse(s.at);
    if (!Number.isFinite(t)) continue;
    x.push(Math.floor(t / 1000));
    pick.forEach((p, i) => ys[i]!.push(p(s) ?? null));
  }
  return { x, ys };
}

/** "3.5 / 8 cores" style ratio text. */
export function ratioText(used: number | null | undefined, total: number | null | undefined, unit: Unit): string {
  const f = unit === "bytes" ? formatBytes : unit === "cores" ? formatCores : (n: number | null | undefined) => (n == null ? "–" : String(n));
  return `${f(used)} / ${f(total)}`;
}

/** Thin allocation bar: used over total, with the ratio as its label. */
export function UsageBar({ used, total, unit, width = 140 }: { used: number | null | undefined; total: number | null | undefined; unit: Unit; width?: number }) {
  const ratio = used != null && total ? Math.min(1, used / total) : 0;
  const tone = ratio >= 0.95 ? "is-hot" : ratio >= 0.8 ? "is-warm" : "";
  return (
    <span className="usage" style={{ width }} title={ratioText(used, total, unit)}>
      <span className={["usage-track", tone].join(" ").trim()}>
        <span className="usage-fill" style={{ width: `${(ratio * 100).toFixed(1)}%` }} />
      </span>
      <span className="usage-text mono">{ratioText(used, total, unit)}</span>
    </span>
  );
}

export const DASH = <span className="muted">–</span>;

export function labelsText(labels: Record<string, string> | undefined): string {
  return Object.entries(labels ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join(" ");
}

/** Pretty-printed JSON block. */
export function JsonBlock({ value }: { value: unknown }) {
  return <pre className="json">{JSON.stringify(value, null, 2)}</pre>;
}
