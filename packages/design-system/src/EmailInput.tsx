import { useId, useRef, type ClipboardEvent, type KeyboardEvent, type ReactNode } from "react";
import { IconClose, IconMinus, IconRefresh, IconWarning } from "./icons.tsx";

/** One address as typed or pasted: email when it parses, name from "Name <email>" or a spreadsheet row. */
export interface EmailEntry {
  /** Stable for the life of the entry (React keys, removal). */
  key: number;
  /** The text it came from, shown when it is not an email. */
  raw: string;
  /** Lower-cased; absent when raw is not an email. */
  email?: string;
  name?: string;
}

/**
 * What the caller says about one valid, first-seen address: invite it, renew
 * its expired invitation, or leave it out with a reason ("already in acme",
 * "already invited 3 days ago") and at most one action ("Copy their link").
 */
export type EmailVerdict =
  | { status: "invite" }
  | { status: "renew"; reason?: string }
  | { status: "skip"; reason: string; icon?: ReactNode; action?: { label: string; onClick: () => void } };

/** A verdict for every entry: the caller's, or the component's own for "not an email" and "twice in the list". */
export type EmailView = EmailVerdict | { status: "invalid"; reason: string } | { status: "duplicate"; reason: string };

const EMAIL_RE = /^[^\s<>@,;"]+@[^\s<>@,;".]+(\.[^\s<>@,;".]+)+$/;
let nextKey = 0;

function entry(text: string, name?: string): EmailEntry {
  const raw = text.trim();
  const m = /^"?([^"<]*?)"?\s*<([^>]*)>$/.exec(raw);
  const email = (m ? m[2]! : raw).trim().toLowerCase();
  const nm = (m ? m[1]!.trim() : "") || name?.trim() || undefined;
  return EMAIL_RE.test(email) ? { key: ++nextKey, raw, email, name: nm } : { key: ++nextKey, raw };
}

/* Splits on commas and semicolons outside quotes and angle brackets ("Faria, Jorge" <j@x> stays whole). */
function splitList(line: string): string[] {
  const out: string[] = [];
  let cur = "";
  let quote = false;
  let angle = false;
  for (const ch of line) {
    if (ch === '"') quote = !quote;
    else if (ch === "<") angle = true;
    else if (ch === ">") angle = false;
    if ((ch === "," || ch === ";") && !quote && !angle) {
      out.push(cur);
      cur = "";
    } else cur += ch;
  }
  out.push(cur);
  return out;
}

/**
 * Text → entries. Lines first; a line with tabs is a spreadsheet row (its
 * cell with an @ is the address, the first other non-empty cell the name);
 * otherwise commas and semicolons separate, and a part without "<" splits on
 * whitespace too. "Name <email>" keeps the name. Addresses are lower-cased.
 */
export function parseEmails(text: string): EmailEntry[] {
  const out: EmailEntry[] = [];
  for (const line of text.split(/\r?\n/)) {
    if (!line.trim()) continue;
    if (line.includes("\t")) {
      const cells = line.split("\t").map((c) => c.trim());
      const at = cells.find((c) => c.includes("@"));
      if (at) out.push(entry(at, cells.find((c) => c && c !== at && !c.includes("@"))));
      else for (const c of cells) if (c) out.push(entry(c));
      continue;
    }
    for (const part of splitList(line)) {
      if (!part.trim()) continue;
      if (part.includes("<")) out.push(entry(part));
      else for (const w of part.trim().split(/\s+/)) out.push(entry(w));
    }
  }
  return out;
}

/** Each entry's view: invalid and duplicates first, then the caller's verdict for the rest. */
export function classifyEmails(entries: readonly EmailEntry[], classify: (e: EmailEntry & { email: string }) => EmailVerdict): EmailView[] {
  const seen = new Set<string>();
  return entries.map((e) => {
    if (!e.email) return { status: "invalid", reason: "not an email" };
    if (seen.has(e.email)) return { status: "duplicate", reason: "twice in the list" };
    seen.add(e.email);
    return classify(e as EmailEntry & { email: string });
  });
}

/** Taken: invited or renewed. */
export function isTaken(v: EmailView): boolean {
  return v.status === "invite" || v.status === "renew";
}

/** The counts the summary line and the submit button carry. */
export function emailCounts(views: readonly EmailView[]): { taken: number; renewed: number; left: number } {
  const taken = views.filter(isTaken).length;
  return { taken, renewed: views.filter((v) => v.status === "renew").length, left: views.length - taken };
}

/** "Invite 3 people", "Invite 1 person", "Nothing to invite". */
export function inviteButtonText(taken: number): string {
  return taken === 0 ? "Nothing to invite" : taken === 1 ? "Invite 1 person" : `Invite ${taken} people`;
}

type Tone = "ok" | "skip" | "bad" | "info";
function tone(v: EmailView): Tone {
  return v.status === "invalid" ? "bad" : v.status === "renew" ? "info" : v.status === "invite" ? "ok" : "skip";
}
function srText(v: EmailView): string {
  if (v.status === "invite") return "will be invited";
  if (v.status === "renew") return "will be renewed";
  return `left out: ${v.reason}`;
}
function glyph(v: EmailView): ReactNode {
  if (v.status === "invalid") return <IconWarning size={12} />;
  if (v.status === "renew") return <IconRefresh size={12} />;
  if (v.status === "duplicate") return <IconMinus size={12} />;
  if (v.status === "skip") return v.icon ?? <IconMinus size={12} />;
  return null;
}

export interface EmailInputProps {
  entries: EmailEntry[];
  onChange: (entries: EmailEntry[]) => void;
  /** The caller's verdict for each valid, first-seen address. */
  classify: (e: EmailEntry & { email: string }) => EmailVerdict;
  /** The input's id, for a <label htmlFor>. */
  id?: string;
  /** The chip list's accessible name. */
  label?: string;
  placeholder?: string;
  /** Shown under the box while it is empty. */
  hint?: ReactNode;
  disabled?: boolean;
}

/**
 * Addresses as chips in an Input-height box that grows to four rows. A chip
 * carries the address only; its state is its tone (left out: struck-through
 * outline; not an email: danger fill and a warning glyph; renewed: an info
 * glyph), and why is said once, in the list under the box.
 */
export function EmailInput({ entries, onChange, classify, id, label = "Emails", placeholder = "ana@acme.dev, Tom Okafor <tom@acme.dev>", hint, disabled }: EmailInputProps) {
  const autoId = useId();
  const inputId = id ?? autoId;
  const hintId = `${inputId}-hint`;
  const input = useRef<HTMLInputElement>(null);
  const views = classifyEmails(entries, classify);
  const { taken, renewed, left } = emailCounts(views);

  const commit = (extra = "") => {
    const el = input.current;
    if (!el) return;
    const text = el.value + extra;
    if (!text.trim()) return;
    el.value = "";
    onChange([...entries, ...parseEmails(text)]);
  };
  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    const v = e.currentTarget.value;
    // Space inside "Name <…" or before an @ is part of the name, not a separator.
    const spaceEnds = e.key === " " && v.includes("@") && !/<[^>]*$/.test(v);
    if ((e.key === "Enter" || e.key === "," || e.key === ";" || e.key === "Tab" || spaceEnds) && v.trim()) {
      if ((e.key === "," || e.key === ";") && (/<[^>]*$/.test(v) || (v.split('"').length - 1) % 2 === 1)) return;
      e.preventDefault();
      commit();
    } else if (e.key === "Backspace" && v === "" && entries.length) {
      e.preventDefault();
      onChange(entries.slice(0, -1));
    }
  };
  const onPaste = (e: ClipboardEvent<HTMLInputElement>) => {
    const text = e.clipboardData.getData("text");
    if (!text) return;
    e.preventDefault();
    commit(text);
  };

  const leftOut = entries.map((en, i) => [en, views[i]!] as const).filter(([, v]) => !isTaken(v));
  return (
    <>
      <div
        className={["emailinput", disabled ? "is-disabled" : ""].join(" ").trim()}
        onClick={(e) => {
          if (e.target === e.currentTarget) input.current?.focus();
        }}
      >
        {entries.length > 0 && (
          <ul className="emailinput-chips" aria-label={label}>
            {entries.map((en, i) => {
              const v = views[i]!;
              const shown = en.email ?? en.raw;
              const t = tone(v);
              return (
                <li key={en.key} className="emailchip" data-tone={t} title={isTaken(v) && v.status === "invite" ? shown : `${shown} · ${v.status === "renew" ? (v.reason ?? "will be renewed") : (v as { reason: string }).reason}`}>
                  {(t === "bad" || t === "info") && <span className="emailchip-icon">{glyph(v)}</span>}
                  <span className="emailchip-text">
                    {en.name && en.email ? (
                      <>
                        <b>{en.name}</b> <span className="emailchip-sub">{en.email}</span>
                      </>
                    ) : (
                      shown
                    )}
                  </span>
                  <span className="sr-only">, {srText(v)}</span>
                  <button
                    type="button"
                    className="emailchip-remove"
                    aria-label={`Remove ${shown}`}
                    disabled={disabled}
                    onClick={() => {
                      onChange(entries.filter((x) => x.key !== en.key));
                      input.current?.focus();
                    }}
                  >
                    <IconClose size={10} />
                  </button>
                </li>
              );
            })}
          </ul>
        )}
        <input
          ref={input}
          id={inputId}
          className="emailinput-input"
          autoComplete="off"
          spellCheck={false}
          disabled={disabled}
          placeholder={entries.length ? "" : placeholder}
          aria-describedby={entries.length === 0 && hint ? hintId : undefined}
          onKeyDown={onKeyDown}
          onPaste={onPaste}
          onBlur={() => commit()}
        />
      </div>
      {entries.length === 0 && hint && (
        <span className="field-hint" id={hintId}>
          {hint}
        </span>
      )}
      {entries.length > 0 && (
        <div className="emailinput-out" aria-live="polite">
          <div className="emailinput-sum">
            <span className="emailinput-count">
              <b className="num">{taken}</b> to invite
              {renewed > 0 && <span className="muted"> ({renewed} renewed)</span>}
              {left > 0 && (
                <>
                  {" · "}
                  <b className="num">{left}</b> left out
                </>
              )}
            </span>
            {left > 0 && (
              <button type="button" className="linkbtn" disabled={disabled} onClick={() => onChange(entries.filter((_, i) => isTaken(views[i]!)))}>
                Remove left out
              </button>
            )}
          </div>
          {leftOut.length > 0 && (
            <ul className="emailinput-left" aria-label="Left out">
              {leftOut.map(([en, v]) => (
                <li key={en.key} data-tone={tone(v)}>
                  <span className="emailchip-icon">{glyph(v)}</span>
                  <span className="emailinput-left-addr">{en.email ?? en.raw}</span>
                  <span className="emailinput-left-why">{(v as { reason: string }).reason}</span>
                  {v.status === "skip" && v.action && (
                    <button type="button" className="linkbtn" onClick={v.action.onClick}>
                      {v.action.label}
                    </button>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </>
  );
}
