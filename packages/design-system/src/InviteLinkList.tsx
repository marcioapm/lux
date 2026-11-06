import type { ReactNode } from "react";
import { Button, IconButton } from "./Button.tsx";
import { useCopy } from "./IdChip.tsx";
import { IconCheck, IconCopy } from "./icons.tsx";

export interface InviteLink {
  email: string;
  name?: string;
  url: string;
  /** Beside the address: an "already invited" badge. */
  note?: ReactNode;
}

/** The text Copy all puts on the clipboard: one "email  link" line each, as `lux invites create` prints. */
export function inviteLinksText(links: readonly InviteLink[]): string {
  return links.map((l) => `${l.email}  ${l.url}`).join("\n");
}

/**
 * An address with break opportunities after "@" and before each ".", so a
 * narrow row wraps it at its parts instead of cutting it.
 */
export function breakableEmail(email: string): ReactNode[] {
  const out: ReactNode[] = [];
  let part = "";
  let n = 0;
  for (const ch of email) {
    if (ch === ".") {
      if (part) out.push(part);
      out.push(<wbr key={n++} />);
      part = ".";
    } else if (ch === "@") {
      out.push(part + "@", <wbr key={n++} />);
      part = "";
    } else part += ch;
  }
  if (part) out.push(part);
  return out;
}

function LinkRow({ link }: { link: InviteLink }) {
  const { copied, copy } = useCopy(link.url);
  return (
    <li>
      <span className="invlinks-email" title={link.email}>
        {link.name ? `${link.name} · ` : ""}
        {breakableEmail(link.email)}
      </span>
      <span className="invlinks-url mono" title={link.url}>
        {link.url}
      </span>
      {link.note != null && <span className="invlinks-note">{link.note}</span>}
      <span className="invlinks-copy">
        <IconButton size="sm" label={copied ? "Copied" : `Copy link for ${link.email}`} onClick={copy}>
          {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
        </IconButton>
      </span>
    </li>
  );
}

/**
 * Invitation links to send by hand: each address with its link and a Copy.
 * The address is never cut: it wraps at "@" and "."; in a narrow list
 * (under 480px) the note (badges) goes on its own line under it.
 */
export function InviteLinkList({ links }: { links: readonly InviteLink[] }) {
  return (
    <ul className="invlinks" aria-label="Invitation links">
      {links.map((l) => (
        <LinkRow key={l.email} link={l} />
      ))}
    </ul>
  );
}

/** Copies every link at once (inviteLinksText). */
export function CopyAllLinks({ links }: { links: readonly InviteLink[] }) {
  const { copied, copy } = useCopy(inviteLinksText(links));
  return (
    <Button icon={copied ? <IconCheck size={14} /> : <IconCopy size={14} />} onClick={copy} disabled={links.length === 0}>
      {copied ? "Copied" : "Copy all"}
    </Button>
  );
}
