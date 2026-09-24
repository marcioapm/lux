import { useState } from "react";
import { Badge, formatBytes, formatDuration, formatRelative, formatTimestamp, IdChip, KeyValue, StatePill, Tabs } from "../../ds/index.ts";
import { api, isRunActive, useNow, useQuery, useSession, type Run } from "../../api/index.ts";
import { DASH, ErrorBlock, ErrorStrip, HostLink, labelsText, PageSkeleton } from "./common.tsx";
import { RunActions } from "./RunActions.tsx";
import { RunOutput } from "./RunOutput.tsx";
import { RunResources } from "./RunResources.tsx";
import { RunEvents, RunSnapshots, RunSpecView } from "./RunTabs.tsx";
import { RunTimeline } from "./RunTimeline.tsx";

type Tab = "output" | "timeline" | "resources" | "events" | "snapshots" | "spec";

export function RunPage({ id }: { id: string }) {
  const session = useSession();
  const now = useNow(5000);
  const [tab, setTab] = useState<Tab>("output");
  const [active, setActive] = useState(true);
  const q = useQuery(
    `run:${id}`,
    async (s) => {
      const r = await api.run(id, s);
      setActive(isRunActive(r.state));
      return r;
    },
    { interval: active ? 3000 : 15_000 },
  );
  const run = q.data;

  if (q.error && !run) {
    return (
      <div className="page">
        <ErrorBlock error={q.error} onRetry={q.refetch} />
      </div>
    );
  }
  if (!run) return <PageSkeleton />;

  const onChanged = (r: Run) => {
    // Action responses carry no placements or usage; keep what we have and refetch.
    q.setData({ ...run, ...r, placements: run.placements, usage: run.usage, resume: undefined });
    setActive(true);
    void q.refetch();
  };
  const showTenant = session.role === "operator";
  const live = isRunActive(run.state);

  return (
    <div className="page">
      <div className="page-head">
        <div className="stack" style={{ gap: 4 }}>
          <div className="row">
            <h1 className="page-title">{run.name || <span className="mono">{run.id}</span>}</h1>
            <StatePill kind="run" state={run.state} activity={run.activity} />
            <Badge mono outline>
              {run.spec.workload.adapter}
            </Badge>
            <Badge mono outline>
              epoch {run.epoch}
            </Badge>
          </div>
          <div className="row page-desc">
            <IdChip value={run.id} prefix="run" />
            {showTenant && <span>tenant {run.tenant}</span>}
            {run.hostId ? (
              <span>
                on <HostLink id={run.hostId} name={run.host} />
              </span>
            ) : null}
            {run.exitCode != null && !run.stateReason?.includes(`exit code ${run.exitCode}`) && <span className="mono">exit {run.exitCode}</span>}
          </div>
          {run.stateReason && <div className="state-reason-lg">{run.stateReason}</div>}
        </div>
        <RunActions run={run} operator={session.role === "operator"} onChanged={onChanged} />
      </div>
      <ErrorStrip error={q.error} />

      <KeyValue
        columns={3}
        className="run-facts"
        items={[
          { key: "Created", value: `${formatTimestamp(run.createdAt)} · ${formatRelative(run.createdAt, now)}` },
          { key: "First started", value: run.firstStartedAt ? `${formatTimestamp(run.firstStartedAt)} · queued ${formatDuration(run.usage?.queueSeconds)}` : DASH },
          { key: "Finished", value: run.finishedAt ? `${formatTimestamp(run.finishedAt)} · ${formatRelative(run.finishedAt, now)}` : DASH },
          { key: "Image", value: run.spec.image.ref ?? (run.spec.image.build ? `built from ${run.spec.image.build.containerfile}` : DASH), mono: true },
          { key: "Resources", value: `${run.spec.resources.cpus ?? "–"} cpus · ${formatBytes(run.spec.resources.memory)} · ${formatBytes(run.spec.resources.disk)} disk`, mono: true },
          { key: "Usage", value: run.usage ? `CPU ${formatDuration(run.usage.cpuSeconds)} · peak mem ${formatBytes(run.usage.peakMemoryBytes)} · ${run.usage.placements} placement${run.usage.placements === 1 ? "" : "s"}` : DASH, mono: true },
          { key: "Session", value: run.sessionId ? <IdChip value={run.sessionId} truncate={24} /> : DASH },
          { key: "Snapshot", value: run.snapshotId ? <IdChip value={run.snapshotId} truncate={24} /> : DASH },
          { key: "Labels", value: Object.keys(run.labels ?? {}).length ? <span className="mono">{labelsText(run.labels)}</span> : DASH },
        ]}
      />

      <Tabs<Tab>
        value={tab}
        onChange={setTab}
        items={[
          { key: "output", label: "Output" },
          { key: "timeline", label: "Timeline", count: run.placements?.length },
          { key: "resources", label: "Resources" },
          { key: "events", label: "Events" },
          { key: "snapshots", label: "Snapshots & artifacts" },
          { key: "spec", label: "Spec" },
        ]}
      />
      {tab === "output" && <RunOutput run={run} />}
      {tab === "timeline" && <RunTimeline run={run} now={now} />}
      {tab === "resources" && <RunResources run={run} />}
      {tab === "events" && <RunEvents run={run} live={live} />}
      {tab === "snapshots" && <RunSnapshots run={run} live={live} />}
      {tab === "spec" && <RunSpecView run={run} />}
    </div>
  );
}
