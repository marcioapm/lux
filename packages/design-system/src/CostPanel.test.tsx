import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { BreakdownTable, LabelChips, LabelFilterPopover, matchValues, SplitBar } from "./CostPanel.tsx";
import { StepPicker } from "./StepPicker.tsx";

const mounted: { el: HTMLElement; root: Root }[] = [];
async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  mounted.push({ el, root });
  await act(async () => root.render(node));
  return el;
}

describe("cost panel components", () => {
  beforeAll(() => {
    GlobalRegistrator.register();
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  });
  afterEach(async () => {
    for (const { el, root } of mounted.splice(0)) {
      await act(async () => root.unmount());
      el.remove();
    }
  });
  afterAll(async () => {
    await GlobalRegistrator.unregister();
  });

  test("LabelChips: the keys asked for first, at most max, the rest counted with their text in the title", async () => {
    const el = await render(<LabelChips labels={{ team: "x", app: "jervasion", "jervasion.repository": "absmartly/abs", z: "1" }} first={["app"]} max={2} />);
    const chips = [...el.querySelectorAll(".label-chip")].map((c) => c.textContent);
    expect(chips).toEqual(["app=jervasion", "jervasion.repository=absmartly/abs", "+2"]);
    expect(el.querySelector(".label-chip.is-more")?.getAttribute("title")).toBe("team=x\nz=1");
    const none = await render(<LabelChips labels={{}} />);
    expect(none.textContent).toBe("–");
  });

  test("SplitBar: each part as long as its share; a faint part is drawn, not dropped; a part with no amount is absent", async () => {
    const el = await render(
      <SplitBar
        currency="USD"
        whole="10"
        parts={[
          { amount: "2.5", color: "red", label: "Compute", faint: true },
          { amount: "7.5", color: "blue", label: "External" },
          { amount: null, color: "green", label: "Other" },
        ]}
      />,
    );
    const parts = [...el.querySelectorAll<HTMLElement>(".split-bar-part")];
    expect(parts.map((p) => [p.style.flexGrow, p.classList.contains("is-faint")])).toEqual([
      ["0.25", true],
      ["0.75", false],
    ]);
    expect(el.querySelector(".split-bar")?.getAttribute("title")).toBe("Compute $2.50 · External $7.50");
  });

  test("BreakdownTable: a row per value with its parts, a revoked pill, a missing part as a dash; a row click reports it", async () => {
    const clicked: string[] = [];
    const el = await render(
      <BreakdownTable
        lead="API key"
        onRowClick={(r) => clicked.push(r.id)}
        rows={[
          { id: "k1", label: "ci-bot", color: "red", currency: "USD", runs: 3, compute: "1", external: null, total: "1", share: 0.5, pill: "revoked" },
          { id: "(none)", label: "Before key tracking", color: "grey", currency: "USD", runs: 1, compute: "1", external: "0", total: "1", share: 0.5, quiet: true },
        ]}
      />,
    );
    const rows = [...el.querySelectorAll("tbody tr")];
    expect(rows[0]!.textContent).toContain("revoked");
    expect([...rows[0]!.querySelectorAll("td")].map((td) => td.textContent)).toEqual(["ci-botrevoked", "3", "$1.00", "–", "$1.00", "50%"]);
    await act(async () => (rows[1] as HTMLElement).click());
    expect(clicked).toEqual(["(none)"]);
  });

  test("LabelFilterPopover: values biggest first, filtered by a search; Apply adds the values picked, or (not set)", async () => {
    const applied: unknown[] = [];
    const keysSeen: string[] = [];
    const el = await render(
      <LabelFilterPopover
        keys={[{ key: "app", runs: 3 }, { key: "repo" }]}
        initialKey="app"
        onKeyChange={(k) => keysSeen.push(k)}
        values={(k) =>
          k === "app"
            ? [
                { value: "dude", amounts: [{ currency: "USD", amount: "7.39" }] },
                { value: "jervasion", amounts: [{ currency: "USD", amount: "50.6" }] },
                { value: "tiny", amounts: [] },
              ]
            : undefined
        }
        notSet={() => [{ currency: "USD", amount: "0.4" }]}
        onApply={(f) => applied.push(f)}
      />,
    );
    await act(async () => (el.querySelector(".filter-add") as HTMLElement).click());
    expect(keysSeen).toEqual(["app"]);
    const names = () => [...el.querySelectorAll(".label-filter-value .mono")].map((n) => n.textContent);
    expect(names()).toEqual(["jervasion", "dude", "tiny", "(not set)"]);
    const apply = () => [...el.querySelectorAll("button")].find((b) => b.textContent === "Apply") as HTMLButtonElement;
    expect(apply().disabled).toBe(true);
    const box = (name: string) => [...el.querySelectorAll(".label-filter-value")].find((l) => l.textContent?.includes(name))!.querySelector("input") as HTMLInputElement;
    await act(async () => box("dude").click());
    await act(async () => box("jervasion").click());
    await act(async () => apply().click());
    expect(applied).toEqual([{ key: "app", values: ["dude", "jervasion"], notSet: false }]);
    expect(el.querySelector(".label-filter-pop")).toBeNull();
    expect(matchValues([{ value: "absmartly/abs", amounts: [] }, { value: "Jervasion", amounts: [{ currency: "USD", amount: "1" }] }, { value: "dude", amounts: [{ currency: "USD", amount: "2" }] }], " JER ").map((v) => v.value)).toEqual(["Jervasion"]);
    expect(matchValues([{ value: "a", amounts: [{ currency: "USD", amount: "1" }] }, { value: "b", amounts: [{ currency: "USD", amount: "2" }] }, { value: "c", amounts: [] }], "").map((v) => v.value)).toEqual(["b", "a", "c"]);
  });

  test("StepPicker: Auto shows what it resolved to; a disabled choice says why and cannot be picked", async () => {
    const picked: string[] = [];
    const el = await render(
      <StepPicker
        value="auto"
        resolved="min/hour"
        onChange={(v) => picked.push(v)}
        options={[{ value: "auto", hint: "each chart picks" }, { value: "minute", hint: "1,440 points" }, { value: "hour" }, { value: "day", disabled: "1 point · too coarse" }]}
        note="what it does"
      />,
    );
    expect(el.querySelector(".select-trigger")?.textContent).toBe("EveryAuto · min/hour");
    await act(async () => (el.querySelector(".select-trigger") as HTMLElement).click());
    const opt = (t: string) => [...el.querySelectorAll('[role="option"]')].find((o) => o.textContent?.startsWith(t)) as HTMLElement;
    expect(opt("Minute").textContent).toBe("Minute1,440 points");
    expect(opt("Day").getAttribute("aria-disabled")).toBe("true");
    await act(async () => opt("Day").click());
    await act(async () => opt("Hour").click());
    expect(picked).toEqual(["hour"]);
    expect(el.querySelector(".select-footer")).toBeNull();
  });
});
