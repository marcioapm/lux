import type { ReactNode } from "react";
import { Avatar, type AvatarSize } from "./Avatar.tsx";
import { Badge } from "./Badge.tsx";

export type TenantRole = "viewer" | "member" | "admin" | "owner";

export const ROLES: readonly TenantRole[] = ["viewer", "member", "admin", "owner"];

export const ROLE_LABEL: Record<TenantRole, string> = { viewer: "Viewer", member: "Member", admin: "Admin", owner: "Owner" };

/** What each role may do, in one sentence: a RoleBadge's title, a role picker's hint. */
export const ROLE_DESC: Record<TenantRole, string> = {
  viewer: "Sees everything in the tenant: Runs, output, servers, hosts, pools and costs. Cannot start or change anything.",
  member: "Viewer, plus starts Runs and manages their own (stop, resume, extend, terminal, session input, terminate) and those of service accounts they own.",
  admin: "Member, plus manages every Run in the tenant, members up to Admin, service accounts, tenant pools and host settings.",
  owner: "Admin, plus grants and removes Owners and changes tenant settings (retention, expiry).",
};

const RANK: Record<TenantRole, number> = { viewer: 0, member: 1, admin: 2, owner: 3 };

/** a is at least b; an unknown role is below every role. */
export function roleAtLeast(a: string | null | undefined, b: TenantRole): boolean {
  return a != null && a in RANK && RANK[a as TenantRole] >= RANK[b];
}

export function isRole(r: unknown): r is TenantRole {
  return typeof r === "string" && r in RANK;
}

/**
 * A tenant role as a Badge: Owner accent, Admin info, Member neutral,
 * Viewer outline. Its title says what the role may do. An unknown role
 * (a newer luxd's) shows its name in the neutral tone.
 */
export function RoleBadge({ role, title }: { role: string; title?: string }) {
  if (!isRole(role)) return <Badge title={title}>{role}</Badge>;
  const tone = role === "owner" ? "accent" : role === "admin" ? "info" : "neutral";
  return (
    <Badge tone={tone} className={role === "viewer" ? "badge-role-viewer" : undefined} title={title ?? ROLE_DESC[role]} data-role={role}>
      {ROLE_LABEL[role]}
    </Badge>
  );
}

export interface PersonProps {
  /** The name shown; a person's name, else their email; a service account's name. */
  name: string;
  /** The line under it: an email, a role, "service account · owner …". Omit for one line. */
  sub?: ReactNode;
  picture?: string;
  serviceAccount?: boolean;
  /** After the name, muted: "(you)". */
  note?: ReactNode;
  /** inline: a small avatar and the name on one line (a table cell beside others: Owner, Invited by). */
  inline?: boolean;
  className?: string;
}

/**
 * Who: an avatar, the name and a sub line, each line ellipsized. A table
 * cell for a person or a service account (Members, a Run's Owner, Cost by
 * member).
 */
export function Person({ name, sub, picture, serviceAccount, note, inline, className }: PersonProps) {
  const size: AvatarSize = inline ? "sm" : "md";
  if (inline) {
    return (
      <span className={["person-inline", className ?? ""].join(" ").trim()} title={name}>
        <Avatar name={name} picture={picture} serviceAccount={serviceAccount} size={size} />
        <span className="person-inline-name">{name}</span>
        {note != null && <span className="muted">{note}</span>}
      </span>
    );
  }
  return (
    <span className={["person", className ?? ""].join(" ").trim()}>
      <Avatar name={name} picture={picture} serviceAccount={serviceAccount} size={size} />
      <span className="person-text">
        <span className="person-name">
          {name}
          {note != null && <span className="muted"> {note}</span>}
        </span>
        {sub != null && sub !== "" && <span className="person-sub">{sub}</span>}
      </span>
    </span>
  );
}
