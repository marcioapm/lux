import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { ApiKey } from "../../api/index.ts";

// KeysTable on a phone: Name is the one column without a width, so it gets
// the table's width less the fixed ones; the table's own floor keeps it
// >= 140px.
let parts: typeof import("./peopleParts.tsx");
let ds: typeof import("@lux/design-system");
withDom(async () => {
  parts = await import("./peopleParts.tsx");
  ds = await import("@lux/design-system");
});

const key: ApiKey = { id: "key_1", name: "laptop", prefix: "lux_u_7Hc2", readOnly: false, createdAt: "2026-11-01T00:00:00Z" };
const fixed = (cols: { width?: number | string }[]) => cols.reduce((n, c) => n + (typeof c.width === "number" ? c.width : 0), 0);

for (const [what, props] of [
  ["personal keys with Rename and Revoke", { onRename: () => {}, onRevoke: () => {} }],
  ["a member's keys, Revoke only", { onRevoke: () => {}, whose: "their" }],
  ["service-account keys (no Access, created by)", { access: false, createdBy: true, onRevoke: () => {} }],
] as const) {
  test(`KeysTable, ${what}: the table's min width leaves Name at least 140px`, async () => {
    const el = document.createElement("div");
    el.style.width = "390px";
    document.body.appendChild(el);
    const root = createRoot(el);
    try {
      await act(async () => root.render(<parts.KeysTable keys={[key]} {...props} />));
      const table = el.querySelector("table")!;
      const min = parseFloat(table.style.minWidth);
      const cols = parts.keyColumns({ keys: [key], ...props });
      expect(cols.filter((c) => c.width == null).map((c) => c.key)).toEqual(["name"]);
      // Name's width at the table's narrowest: its min width less the fixed columns.
      expect(min - fixed(cols)).toBeGreaterThanOrEqual(140);
      expect(min).toBe(ds.tableFloor(cols));
    } finally {
      await act(async () => root.unmount());
      el.remove();
    }
  });
}
