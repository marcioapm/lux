// Pieces the people pages share: who cells, key tables, the New key and
// shown-once dialogs, role pickers.
import { useState, type ReactNode } from "react";
import { Badge, Button, Dialog, IconButton, Person, RelativeTime, ROLE_DESC, ROLE_LABEL, ROLES, roleAtLeast, Select, Table, useCopy, type Column, type SelectOption, type TenantRole } from "@lux/design-system";
import { IconCheck, IconCopy, IconPencil, IconTrash } from "@lux/design-system/icons";
import type { ApiKey, CreatedApiKey, NewKeyRequest, Owner, Person as PersonT } from "../../api/index.ts";


export const memberPath = (id: string) => `/members/${encodeURIComponent(id)}`;
export const serviceAccountPath = (id: string) => `/service-accounts/${encodeURIComponent(id)}`;

/** A Run's, key's or cost row's owner on one line; "–" with no owner. */
export function OwnerCell({ owner, me }: { owner?: Owner; me?: string }) {
  if (!owner) return <span className="muted" title="No owner: from before lux had accounts, its key matched no person or service account">–</span>;
  return <Person inline name={owner.name || owner.email || owner.id} serviceAccount={owner.kind === "serviceAccount"} note={owner.kind === "user" && me && owner.id === me ? "(you)" : undefined} />;
}

export function PersonInline({ person }: { person?: PersonT }) {
  if (!person) return <span className="muted">–</span>;
  return <Person inline name={person.name || person.email} picture={person.picture} />;
}

/** The access a key has, in words. */
function accessText(k: ApiKey, role: TenantRole | null, whose: string): ReactNode {
  if (k.readOnly) return <Badge className="badge-role-viewer">Read-only</Badge>;
  return <span className="muted">{role ? `${ROLE_LABEL[role]} (${whose} role)` : `Same as ${whose} role`}</span>;
}

function expiresCell(k: ApiKey) {
  if (!k.expiresAt) return <span className="muted">never</span>;
  const soon = Date.parse(k.expiresAt) - Date.now() < 14 * 86_400_000;
  return soon ? (
    <Badge tone="warn">
      <RelativeTime at={k.expiresAt} />
    </Badge>
  ) : (
    <RelativeTime at={k.expiresAt} />
  );
}

export interface KeysTableProps {
  keys: ApiKey[];
  loading?: boolean;
  /** The role keys that follow it have: "Member (my role)". */
  role?: TenantRole | null;
  whose?: string;
  /** Hide Access (service-account keys follow the account's role). */
  access?: boolean;
  createdBy?: boolean;
  onRename?: (k: ApiKey) => void;
  onRevoke?: (k: ApiKey) => void;
  empty?: ReactNode;
}

export function KeysTable(props: KeysTableProps) {
  const { keys, loading, empty } = props;
  // No minWidth: the Table's floor (fixed widths plus 140px for Name, the
  // one column without a width) keeps Name readable on a phone, where a
  // forced 720 left it 22px; below the floor the table scrolls sideways.
  return <Table columns={keyColumns(props)} rows={keys} rowKey={(k) => k.id} loading={loading} defaultSort={{ key: "created", dir: "desc" }} empty={empty ?? "No API keys."} />;
}

/** KeysTable's columns: Name (no width: the rest of the table), then fixed ones. */
export function keyColumns({ role = null, whose = "my", access = true, createdBy, onRename, onRevoke }: KeysTableProps): Column<ApiKey>[] {
  const cols: Column<ApiKey>[] = [
    { key: "name", header: "Name", cell: (k) => k.name, sortValue: (k) => k.name, lead: true },
    { key: "prefix", header: "Key", cell: (k) => <span className="mono muted">{k.prefix}…</span>, width: 130 },
    ...(access ? [{ key: "access", header: "Access", cell: (k: ApiKey) => accessText(k, role, whose), width: 160 }] : []),
    { key: "created", header: "Created", cell: (k) => (createdBy && k.createdBy ? <span className="row" style={{ gap: 6, justifyContent: "flex-end" }}><RelativeTime at={k.createdAt} /><span className="muted">· {k.createdBy.name.split(" ")[0]}</span></span> : <RelativeTime at={k.createdAt} />), sortValue: (k) => Date.parse(k.createdAt), align: "right", width: createdBy ? 150 : 110 },
    { key: "used", header: "Last used", cell: (k) => <RelativeTime at={k.lastUsedAt} />, sortValue: (k) => (k.lastUsedAt ? Date.parse(k.lastUsedAt) : null), align: "right", width: 104 },
    { key: "expires", header: "Expires", cell: expiresCell, sortValue: (k) => (k.expiresAt ? Date.parse(k.expiresAt) : Infinity), align: "right", width: 110 },
  ];
  if (onRename || onRevoke)
    cols.push({
      key: "actions",
      header: "",
      label: "Actions",
      cell: (k) => (
        <span className="row-actions">
          {onRename && (
            <IconButton size="sm" label={`Rename ${k.name}`} onClick={() => onRename(k)}>
              <IconPencil size={14} />
            </IconButton>
          )}
          {onRevoke && (
            <IconButton size="sm" label={`Revoke ${k.name}`} onClick={() => onRevoke(k)}>
              <IconTrash size={14} />
            </IconButton>
          )}
        </span>
      ),
      align: "right",
      width: onRename && onRevoke ? 84 : 52,
    });
  return cols;
}

const EXPIRY = [
  { value: "never", label: "Never" },
  { value: "30", label: "In 30 days" },
  { value: "90", label: "In 90 days" },
  { value: "365", label: "In a year" },
] as const;

/** New key: name, access (personal keys), expiry; onCreate resolves with the key, which the caller then reveals. */
export function NewKeyDialog({ open, who, readOnlyChoice, onCreate, onCancel }: { open: boolean; who: ReactNode; readOnlyChoice?: { roleLabel: string }; onCreate: (body: NewKeyRequest) => Promise<void>; onCancel: () => void }) {
  const [name, setName] = useState("");
  const [access, setAccess] = useState<"role" | "read">("role");
  const [exp, setExp] = useState<string>("90");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const reset = () => {
    setName("");
    setAccess("role");
    setExp("90");
    setError(null);
  };
  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      await onCreate({ name: name.trim(), readOnly: access === "read" || undefined, ...(exp !== "never" ? { expiresAt: new Date(Date.now() + Number(exp) * 86_400_000).toISOString() } : {}) });
      reset();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      open={open}
      title="New API key"
      description={who}
      confirmLabel="Create key"
      disabled={name.trim() === ""}
      loading={busy}
      width={520}
      onConfirm={() => void submit()}
      onCancel={() => {
        reset();
        onCancel();
      }}
    >
      <label className="field">
        <span className="field-label">Name</span>
        <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="ci-laptop" autoComplete="off" spellCheck={false} autoFocus />
        <span className="field-hint">Where it lives, so you know which to revoke.</span>
      </label>
      {readOnlyChoice && (
        <div className="field">
          <span className="field-label">Access</span>
          <Select<"role" | "read">
            value={access}
            onChange={setAccess}
            options={[
              { value: "role", label: `Same as my role (${readOnlyChoice.roleLabel})`, text: "Same as my role" },
              { value: "read", label: "Read-only", text: "Read-only" },
            ]}
          />
          <span className="field-hint">{access === "read" ? "Reads only, whatever your role." : "Follows your role: if an admin changes it, the key changes with it."}</span>
        </div>
      )}
      <div className="field">
        <span className="field-label">Expires</span>
        <Select value={exp} onChange={setExp} options={EXPIRY.map((e) => ({ value: e.value, label: e.label, text: e.label }))} />
      </div>
      {error && (
        <div className="error-strip" role="alert">
          {error}
        </div>
      )}
    </Dialog>
  );
}

/** The key's secret, once: copy it, and how to use it. */
export function KeyRevealDialog({ created, onDone }: { created: CreatedApiKey | null; onDone: () => void }) {
  const value = created?.apiKey ?? "";
  const { copied, copy } = useCopy(value);
  return (
    <Dialog open={created != null} title="Key created" description="Copy it now: lux keeps only a hash and cannot show it again." confirmLabel="Done" cancelLabel="Close" width={520} onConfirm={onDone} onCancel={onDone}>
      <div className="secret-box">
        <input className="input mono" readOnly value={value} aria-label="The new API key" onFocus={(e) => e.currentTarget.select()} />
        <Button icon={copied ? <IconCheck size={14} /> : <IconCopy size={14} />} onClick={copy}>
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <div className="field">
        <span className="field-label">Use it</span>
        <pre className="snippet">{`export LUX_API_KEY=${value}\nlux whoami`}</pre>
      </div>
    </Dialog>
  );
}

/** Role options up to what the actor may grant: Owner off, with the reason, unless they are an owner. */
export function roleOptions(actor: { owner: boolean }, { includeOwner = true }: { includeOwner?: boolean } = {}): SelectOption<TenantRole>[] {
  const roles = includeOwner ? ROLES : ROLES.filter((r) => r !== "owner");
  return roles.map((r) => ({ value: r, label: ROLE_LABEL[r], text: ROLE_LABEL[r], ...(r === "owner" && !actor.owner ? { disabled: "only owners grant Owner" } : {}) }));
}

export function RoleField({ value, onChange, options, label = "Role" }: { value: TenantRole; onChange: (r: TenantRole) => void; options: SelectOption<TenantRole>[]; label?: string }) {
  return (
    <div className="field">
      <span className="field-label">{label}</span>
      <Select<TenantRole> value={value} onChange={onChange} options={options} />
      <span className="field-hint">{ROLE_DESC[value]}</span>
    </div>
  );
}

export { roleAtLeast };
