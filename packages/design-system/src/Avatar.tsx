import { useState, type ReactNode } from "react";
import { IconBot } from "./icons.tsx";

export type AvatarSize = "sm" | "md" | "lg";

export interface AvatarProps {
  /** Who it is: initials come from it, and it is the accessible name when labelled. */
  name: string;
  /** A photo's URL (the identity provider's); initials when absent or when it fails to load. */
  picture?: string;
  /** Instead of initials (a key glyph for a key session). */
  fallback?: ReactNode;
  /** sm 22px (a table cell beside a name), md 30px (default), lg 48px (a page header). */
  size?: AvatarSize;
  /** A service account: a square with the Bot glyph, never a photo or initials. */
  serviceAccount?: boolean;
  /** It names the person (where their name is not shown beside it); otherwise it is decoration. */
  labelled?: boolean;
  className?: string;
}

const BOT_SIZE: Record<AvatarSize, number> = { sm: 12, md: 15, lg: 22 };

/** A person's photo or initials, or a service account's square. */
export function Avatar({ name, picture, fallback, size = "md", serviceAccount, labelled, className }: AvatarProps) {
  const [failed, setFailed] = useState<string | null>(null);
  const cls = ["avatar", size !== "md" ? `avatar-${size}` : "", serviceAccount ? "avatar-sa" : "", className].filter(Boolean).join(" ");
  const a11y = labelled ? { role: "img", "aria-label": name } : { "aria-hidden": true as const };
  let inner: ReactNode;
  if (serviceAccount) inner = <IconBot size={BOT_SIZE[size]} />;
  else if (picture && failed !== picture) inner = <img src={picture} alt="" referrerPolicy="no-referrer" onError={() => setFailed(picture)} />;
  else inner = fallback ?? initials(name);
  return (
    <span className={cls} {...a11y}>
      {inner}
    </span>
  );
}

/* Graphemes where the browser can (an emoji or accent stays whole); code points otherwise. */
const graphemes = typeof Intl.Segmenter === "function" ? new Intl.Segmenter() : null;
function firstChar(w: string): string {
  return (graphemes ? graphemes.segment(w).containing(0)?.segment : [...w][0]) ?? "";
}

/** "Ada Lovelace" → "AL"; "ada@example.com" → "A"; "Ada Lovelace (ops)" → "AO"; "Ada | Ops" → "AO". Leading punctuation and symbols are skipped; emoji (So) are kept. */
export function initials(name: string): string {
  const local = /^[^\s@]+@[^\s@]+$/.test(name) ? name.slice(0, name.indexOf("@")) : name;
  const words = local
    .split(/[\s._-]+/)
    .map((w) => w.replace(/^[\p{P}\p{Sm}\p{Sc}\p{Sk}]+/u, ""))
    .filter(Boolean);
  const ends = words.length > 1 ? [words[0] ?? "", words.at(-1) ?? ""] : words;
  return ends.map(firstChar).join("").toUpperCase() || "?";
}
