// Folds a Run's output records into a Session, incrementally: fold() takes
// each new batch onto the state the earlier ones built, so a long Run is
// never replayed per record. Items are replaced, never mutated, when they
// change, and snapshot() copies only the threads a fold touched, so React
// can skip everything else by identity.
import type { Event } from "../../../api/index.ts";
import type { Adapter, OutputRecord, Session, SessionItem, SessionThread, SubagentSupport, ToolItem } from "./types.ts";
import { foldACP } from "./acp.ts";
import { foldClaude } from "./claude.ts";
import { foldCodex } from "./codex.ts";
import { foldLux, foldLuxEvent } from "./lux.ts";
import { foldOpenCode } from "./opencode.ts";

export const ROOT = "root";

// What each adapter can say about subagents (docs/adapters.md; codex: the
// app-server's subAgentActivity item and the child thread's notifications).
export function subagentSupport(adapter: Adapter): SubagentSupport {
  return adapter === "opencode" || adapter === "claude-code" || adapter === "codex" ? "reported" : "unsupported";
}

export class Builder {
  readonly threads = new Map<string, SessionThread>();
  // Where each item sits in its thread: thread id -> item id -> index.
  private readonly index = new Map<string, Map<string, number>>();
  private touched = new Set<string>();
  // Adapter-private state the mappers keep between folds.
  readonly memo = new Map<string, unknown>();
  sessionId?: string;
  model?: string;
  activity?: "busy" | "idle";
  turns = 0;
  compactions = 0;
  records = 0;
  cursor = "";
  version = 0;
  // The record being folded: its time and cursor.
  t = 0;
  at = "";

  constructor(readonly adapter: Adapter) {
    this.threads.set(ROOT, { id: ROOT, title: "Run session", status: "idle", items: [], children: [], tools: 0 });
    this.index.set(ROOT, new Map());
  }

  thread(id: string, init?: Partial<SessionThread>): SessionThread {
    let th = this.threads.get(id);
    if (!th) {
      th = { id, title: "Subagent", status: "running", items: [], children: [], tools: 0, ...init };
      this.threads.set(id, th);
      this.index.set(id, new Map());
      this.touch(id);
    }
    return th;
  }

  has(id: string): boolean {
    return this.threads.has(id);
  }

  touch(id: string) {
    this.touched.add(id);
  }

  // Sets fields of a thread (status, title, …) as a new object.
  setThread(id: string, patch: Partial<SessionThread>) {
    const th = this.thread(id);
    Object.assign(th, patch);
    this.touch(id);
  }

  // Links child under parent once, in first-seen order.
  adopt(parent: string, child: string) {
    const p = this.thread(parent);
    const c = this.thread(child);
    if (c.parent && c.parent !== parent) return;
    c.parent = parent;
    if (!p.children.includes(child)) {
      p.children = [...p.children, child];
      this.touch(parent);
    }
  }

  item<T extends SessionItem>(thread: string, id: string): T | undefined {
    const i = this.index.get(thread)?.get(id);
    return i == null ? undefined : (this.threads.get(thread)!.items[i] as T);
  }

  push(thread: string, item: SessionItem) {
    const th = this.thread(thread);
    this.index.get(thread)!.set(item.id, th.items.length);
    th.items.push(item);
    if (item.kind === "tool") th.tools++;
    this.touch(thread);
  }

  // Puts an item in time order: lux's lifecycle events arrive after the
  // records they sit between.
  insert(thread: string, item: SessionItem) {
    const th = this.thread(thread);
    let at = th.items.length;
    while (at > 0 && th.items[at - 1]!.t > item.t) at--;
    if (at === th.items.length) return this.push(thread, item);
    th.items.splice(at, 0, item);
    const idx = this.index.get(thread)!;
    th.items.forEach((x, i) => idx.set(x.id, i));
    this.touch(thread);
  }

  update<T extends SessionItem>(thread: string, id: string, patch: Partial<T>): T | undefined {
    const i = this.index.get(thread)?.get(id);
    if (i == null) return undefined;
    const th = this.threads.get(thread)!;
    const next = { ...th.items[i]!, ...patch } as T;
    th.items[i] = next;
    this.touch(thread);
    return next;
  }

  last(thread: string): SessionItem | undefined {
    const items = this.threads.get(thread)?.items;
    return items?.[items.length - 1];
  }

  // Ends whatever is streaming in a thread: a reply's caret stops at the
  // next item, or at the turn's end.
  settle(thread: string) {
    const l = this.last(thread);
    if (l?.kind === "message" && l.streaming) this.update(thread, l.id, { streaming: false });
  }

  // Tool calls since the thread's last turn marker, and when the turn began.
  turnStats(thread: string): { tools: number; subagents: number; start?: number } {
    const items = this.threads.get(thread)?.items ?? [];
    let tools = 0;
    let subagents = 0;
    let start: number | undefined;
    for (let i = items.length - 1; i >= 0; i--) {
      const x = items[i]!;
      if (x.kind === "marker" && x.type === "turn") break;
      if (x.kind === "tool") {
        tools++;
        if ((x as ToolItem).child) subagents++;
      }
      if (x.kind !== "marker") start = x.t;
    }
    return { tools, subagents, start };
  }

  snapshot(prev?: Session): Session {
    const threads: Record<string, SessionThread> = {};
    for (const [id, th] of this.threads) {
      const old = prev?.threads[id];
      threads[id] = old && !this.touched.has(id) ? old : { ...th, items: th.items.slice(), children: th.children.slice() };
    }
    const changed = this.touched.size > 0 || !prev;
    this.touched = new Set();
    if (changed) this.version++;
    return {
      adapter: this.adapter,
      subagents: subagentSupport(this.adapter),
      root: ROOT,
      threads,
      sessionId: this.sessionId,
      model: this.model,
      activity: this.activity,
      turns: this.turns,
      compactions: this.compactions,
      records: this.records,
      cursor: this.cursor,
      version: this.version,
    };
  }
}

// The data of an event record: {type, data}.
export function eventOf(r: OutputRecord): { type: string; data: Record<string, unknown> } | null {
  const ev = r.event as { type?: unknown; data?: unknown } | undefined;
  if (r.ch !== "event" || !ev || typeof ev.type !== "string") return null;
  const data = ev.data && typeof ev.data === "object" && !Array.isArray(ev.data) ? (ev.data as Record<string, unknown>) : {};
  return { type: ev.type, data };
}

export class SessionProjection {
  private readonly b: Builder;
  private last?: Session;

  constructor(adapter: Adapter) {
    this.b = new Builder(adapter);
  }

  fold(records: OutputRecord[]): this {
    const b = this.b;
    for (const r of records) {
      b.records++;
      b.cursor = r.cursor;
      b.t = r.t;
      b.at = r.cursor;
      const ev = eventOf(r);
      if (!ev) continue;
      if (foldLux(b, ev.type, ev.data)) continue;
      switch (b.adapter) {
        case "acp":
          foldACP(b, ev.type, ev.data);
          break;
        case "opencode":
          if (!foldOpenCode(b, ev.type, ev.data)) foldACP(b, ev.type, ev.data);
          break;
        case "claude-code":
          foldClaude(b, ev.type, ev.data);
          break;
        case "codex":
          foldCodex(b, ev.type, ev.data);
          break;
      }
    }
    return this;
  }

  // A lux lifecycle event (the output stream's "lux" events).
  foldEvent(e: Event): this {
    this.b.t = Date.parse(e.time);
    foldLuxEvent(this.b, e);
    return this;
  }

  snapshot(): Session {
    this.last = this.b.snapshot(this.last);
    return this.last;
  }
}

// The whole of a Run's records at once (tests, a settled Run).
export function sessionFromRecords(adapter: Adapter, records: OutputRecord[]): Session {
  return new SessionProjection(adapter).fold(records).snapshot();
}
