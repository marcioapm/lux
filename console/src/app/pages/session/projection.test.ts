import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { OutputRecord } from "../../../api/index.ts";
import { sessionFromRecords, SessionProjection } from "./projection.ts";
import type { InputItem, MessageItem, PlanItem, SessionItem, ThoughtItem, ToolItem } from "./types.ts";

// Records lux wrote for a real run, recorded on this repo's host (testdata/).
export function fixture(name: string): OutputRecord[] {
  return readFileSync(new URL(`./testdata/${name}`, import.meta.url), "utf8")
    .split("\n")
    .filter((l) => l.trim() !== "")
    .map((l) => JSON.parse(l) as OutputRecord);
}

const kinds = (items: SessionItem[]) => items.map((i) => (i.kind === "marker" ? `marker:${i.type}` : i.kind));

test("acp: an OpenCode turn over plain ACP reads as input, thought, plan, tools, reply, turn end", () => {
  const s = sessionFromRecords("acp", fixture("acp-opencode-1.18.21-todo-task.jsonl"));
  const root = s.threads[s.root]!;
  expect(kinds(root.items)).toEqual(["input", "thought", "plan", "tool", "thought", "tool", "message", "marker:turn"]);

  const input = root.items[0] as InputItem;
  expect(input.steer).toBe(false);
  expect(input.phase).toBe("accepted");
  expect(input.text.startsWith("First write a todo list")).toBe(true);

  expect((root.items[1] as ThoughtItem).text).toBe("Let me follow the instructions. First write a todo list with two items, then use task tool.");

  // The second todowrite ticks off the first plan; it does not add another.
  expect((root.items[2] as PlanItem).steps).toEqual([
    { text: "Run 'wc -l notes.txt' via a general subagent and get the count", status: "done" },
    { text: "Run 'ls' and mark todos done", status: "done" },
  ]);

  const task = root.items[3] as ToolItem;
  expect([task.name, task.title, task.status, task.cursor]).toEqual(["task", "Run wc -l notes.txt", "completed", "1.15"]);
  const ls = root.items[5] as ToolItem;
  expect([ls.name, ls.title, ls.status, ls.output]).toEqual(["bash", "ls", "completed", "notes.txt\n"]);
  expect((ls.input as { command: string }).command).toBe("ls");

  const reply = root.items[6] as MessageItem;
  expect(reply.text).toBe("`notes.txt` has 3 lines, and `ls` shows only `notes.txt` in the directory.");
  expect(reply.streaming).toBe(false);
  const end = root.items[7]!;
  expect(end.kind === "marker" && end.text).toBe("turn ended · 10.5s · 2 tool calls");

  expect(s.subagents).toBe("unsupported");
  expect(s.sessionId).toBe("ses_ec59d54c8ffeodNJkSlnW18KML");
  expect(s.activity).toBe("idle");
  expect([s.turns, s.records, s.cursor]).toEqual([1, 46, "1.46"]);
});

test("folding in batches gives the same session as folding at once", () => {
  const recs = fixture("acp-opencode-1.18.21-todo-task.jsonl");
  const whole = sessionFromRecords("acp", recs);
  const p = new SessionProjection("acp");
  for (let i = 0; i < recs.length; i += 7) p.fold(recs.slice(i, i + 7)).snapshot();
  const inBatches = p.snapshot();
  expect(inBatches.threads).toEqual(whole.threads);
  expect(inBatches.cursor).toBe(whole.cursor);
});

test("a reply streams with a caret until the next item", () => {
  const recs = fixture("acp-opencode-1.18.21-todo-task.jsonl");
  const p = new SessionProjection("acp");
  const mid = p.fold(recs.slice(0, 37)).snapshot();
  const last = mid.threads[mid.root]!.items.at(-1) as MessageItem;
  expect([last.kind, last.streaming, last.text]).toEqual(["message", true, "`notes.txt` has 3 lines, and `"]);
});

test("a fold that changes nothing keeps every thread object", () => {
  const recs = fixture("acp-opencode-1.18.21-todo-task.jsonl");
  const p = new SessionProjection("acp");
  const a = p.fold(recs).snapshot();
  const b = p.fold([]).snapshot();
  expect(b.threads[b.root]).toBe(a.threads[a.root]!);
  expect(b.version).toBe(a.version);
});
