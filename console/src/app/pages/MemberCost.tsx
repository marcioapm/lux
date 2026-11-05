import { useState } from "react";
import { Card, compareMoney, Meter, Money, Person, SegmentedControl, sumMoney, Table, type Column } from "@lux/design-system";
import { api, useQuery, type CostByOwnerRow } from "../../api/index.ts";
import { useScope } from "../scope.tsx";
import { ErrorBlock } from "./common.tsx";

/** Overview › Cost: each person and service account, or service accounts folded into their owner. */
export function MemberCost() {
  const { apiTenant, range } = useScope();
  const [by, setBy] = useState<"member" | "owner">("member");
  const q = useQuery(`costs:people@${apiTenant ?? ""}:${range}:${by}`, (s) => api.costsByOwner(apiTenant, by, range, s));
  const rows = q.data?.totals ?? [];
  const whole = (currency: string) => sumMoney(rows.filter((r) => r.currency === currency).map((r) => r.amount));
  const cols: Column<CostByOwnerRow>[] = [
    { key: "who", header: by === "member" ? "Member" : "Owner", cell: (r) => <Person name={r.owner?.name || r.owner?.email || "(no owner)"} serviceAccount={r.owner?.kind === "serviceAccount" || !r.owner} sub={r.owner?.kind === "user" ? r.owner.email : r.owner ? "service account" : "Runs from before accounts, key unmatched"} />, sortValue: (r) => r.owner?.name ?? "(no owner)" },
    { key: "runs", header: "Runs", cell: (r) => r.runs?.toLocaleString("en-US") ?? "–", sortValue: (r) => r.runs, align: "right", mono: true, width: 100 },
    { key: "cost", header: "Cost", cell: (r) => <Money amount={r.amount} currency={r.currency} />, sortValue: (r) => Number(r.amount), align: "right", mono: true, width: 130 },
    { key: "share", header: "Share", cell: (r) => <span className="share-cell"><Meter value={Number(r.amount) / Number(whole(r.currency))} /></span>, width: 180 },
  ];
  const sorted = [...rows].sort((a, b) => a.currency.localeCompare(b.currency) || compareMoney(b.amount, a.amount));
  return (
    <Card flush title="Cost by member" subtitle={by === "member" ? "people and service accounts, each on its own row · list prices" : "each service account folded into its owner · list prices"} actions={<SegmentedControl label="Cost attribution" value={by} onChange={setBy} options={[{ value: "member", label: "By member" }, { value: "owner", label: "By owner" }]} />}>
      {q.error && !q.data ? <ErrorBlock error={q.error} onRetry={q.refetch} compact /> : <Table columns={cols} rows={sorted} rowKey={(r) => `${r.key}:${r.currency}`} loading={q.loading} empty="No member cost reported yet." />}
    </Card>
  );
}
