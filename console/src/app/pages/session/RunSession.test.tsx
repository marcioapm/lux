import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Run } from "../../../api/index.ts";
import { fixture } from "./fixtures.ts";
import { RunSession } from "./RunSession.tsx";

beforeAll(() => {
  GlobalRegistrator.register({ url: "http://localhost/runs/r1", width: 1600 });
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(() => GlobalRegistrator.unregister());

const run = (adapter: string): Run => ({ id: "r1", tenant: "t", labels: {}, state: "succeeded", epoch: 1, spec: { workload: { adapter } }, secrets: [], createdAt: new Date().toISOString() }) as unknown as Run;

// Serves a fixture as the output stream the tab reads, then ends it.
async function render(adapter: string, file: string) {
  const original = globalThis.fetch;
  const urls: string[] = [];
  const body = fixture(file)
    .map((r) => `event: record\ndata: ${JSON.stringify(r)}\n\n`)
    .join("") + "event: end\ndata: {}\n\n";
  globalThis.fetch = (async (url) => {
    urls.push(String(url));
    return new Response(body, { headers: { "Content-Type": "text/event-stream" } });
  }) as typeof fetch;
  const el = document.createElement("div");
  document.body.append(el);
  const root = createRoot(el);
  await act(async () => {
    root.render(<RunSession run={run(adapter)} />);
    await Bun.sleep(150);
  });
  return {
    el,
    urls,
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      globalThis.fetch = original;
    },
  };
}

test("the tab reads the Run's output stream and shows its subagent in the transcript and the rail", async () => {
  const v = await render("claude-code", "claude-2.1.207-agent-subagent.jsonl");
  try {
    expect(v.urls[0]).toContain("/runs/r1/output");
    expect(v.urls[0]).toContain("events=true");
    const text = v.el.textContent ?? "";
    expect(text).toContain("The first line of /etc/hosts is ## (a comment line with two hash marks).");
    expect(text).toContain("Read-only · input and interrupt are on Output");
    expect(v.el.querySelector('[role="tree"]')?.textContent).toContain("Read /etc/hosts first line");
    const stub = v.el.querySelector<HTMLElement>(".subthread")!;
    expect(stub.getAttribute("aria-label")).toBe("Open subagent Read /etc/hosts first line");
    await act(async () => stub.click());
    const panel = v.el.querySelector(".subpanel")!;
    expect(panel.querySelector(".subpanel-title")?.textContent).toBe("Read /etc/hosts first line");
    expect(panel.querySelector(".subpanel-brief")?.textContent).toBe("Read the file /etc/hosts and report only its first line. Do not read anything else.");
    expect(panel.querySelector(".toolcall-name")?.textContent).toBe("Read");
  } finally {
    await v.done();
  }
});

test("an acp Run's rail says its adapter doesn't report subagents", async () => {
  const v = await render("acp", "acp-opencode-1.18.21-todo-task.jsonl");
  try {
    expect(v.el.textContent).toContain("The acp adapter doesn't report subagents: ACP shows only the Run's own session.");
    expect(v.el.querySelector('[role="tree"]')).toBeNull();
  } finally {
    await v.done();
  }
});
