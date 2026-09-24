import { useMemo } from "react";
import { Card, TimeSeriesChart, type ChartMark } from "../../ds/index.ts";
import { api, useQuery, type Run } from "../../api/index.ts";
import { ErrorBlock, seriesFrom } from "./common.tsx";

export function RunResources({ run }: { run: Run }) {
  // Cover the Run's whole life (plus slack), so a young Run gets raw samples
  // rather than minute rollups that do not exist yet.
  const since = useMemo(() => {
    const ageMin = Math.ceil((Date.now() - Date.parse(run.createdAt)) / 60_000) + 2;
    return ageMin < 60 * 48 ? `${ageMin}m` : `${Math.ceil(ageMin / 1440)}d`;
  }, [run.createdAt]);
  const q = useQuery(`run-history:${run.id}:${since}`, (s) => api.runHistory(run.id, since, s), { interval: 15_000 });
  const samples = q.data?.samples;

  const cpu = useMemo(() => seriesFrom(samples, [(s) => s.cpuCores, () => run.spec.resources.cpus ?? null]), [samples, run.spec.resources.cpus]);
  const mem = useMemo(() => seriesFrom(samples, [(s) => s.memoryBytes, () => run.spec.resources.memory ?? null]), [samples, run.spec.resources.memory]);
  const disk = useMemo(() => seriesFrom(samples, [(s) => s.diskBytes, () => run.spec.resources.disk ?? null]), [samples, run.spec.resources.disk]);
  const pids = useMemo(() => seriesFrom(samples, [(s) => s.pids]), [samples]);
  const net = useMemo(() => seriesFrom(samples, [(s) => s.netRxRate, (s) => s.netTxRate]), [samples]);

  const marks = useMemo<ChartMark[]>(() => {
    const out: ChartMark[] = [];
    let last: number | undefined;
    for (const s of samples ?? []) {
      if (s.epoch != null && s.epoch !== last) {
        if (last != null) out.push({ x: Math.floor(Date.parse(s.at) / 1000), label: `epoch ${s.epoch}` });
        last = s.epoch;
      }
    }
    return out;
  }, [samples]);

  if (q.error && !q.data) return <ErrorBlock error={q.error} onRetry={q.refetch} />;
  const limit = { label: "Requested", color: "var(--fg-faint)", dashed: true } as const;
  return (
    <div className="stack">
      <div className="muted">
        {q.data ? `${q.data.samples.length} samples over ${since}${q.data.resolution ? ` at ${q.data.resolution}s` : ""}` : "Loading…"}
        {marks.length > 0 && " · dashed verticals mark placement epochs"}
      </div>
      <div className="grid grid-2">
        <Card title="CPU" subtitle="cores used">
          <TimeSeriesChart x={cpu.x} ys={cpu.ys} series={[{ label: "Used", color: 1, area: true }, limit]} unit="cores" height={180} marks={marks} />
        </Card>
        <Card title="Memory">
          <TimeSeriesChart x={mem.x} ys={mem.ys} series={[{ label: "Used", color: 7, area: true }, limit]} unit="bytes" height={180} marks={marks} />
        </Card>
        <Card title="Disk">
          <TimeSeriesChart x={disk.x} ys={disk.ys} series={[{ label: "Used", color: 4, area: true }, limit]} unit="bytes" height={180} marks={marks} />
        </Card>
        <Card title="Processes">
          <TimeSeriesChart x={pids.x} ys={pids.ys} series={[{ label: "Pids", color: 3, step: true, area: true }]} unit="count" height={180} marks={marks} />
        </Card>
        <Card title="Network" subtitle="bytes per second">
          <TimeSeriesChart x={net.x} ys={net.ys} series={[{ label: "Received", color: 1 }, { label: "Sent", color: 2 }]} unit="rate" height={180} marks={marks} />
        </Card>
      </div>
    </div>
  );
}
