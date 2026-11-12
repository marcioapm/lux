# Security

What lux isolates, what it trusts, and what an operator must do. Details
are in the linked pages.

## Tenants

- **Row-level security.** Every tenant row (Runs, placements, events,
  snapshots, blobs, keys, samples) is guarded by Postgres row-level
  security. luxd connects as `lux_app`, a role that cannot bypass it
  (`migrate` makes sure: no SUPERUSER, no BYPASSRLS), and sets the tenant
  per transaction. A query without a tenant sees nothing.
- **Hosts** belong to a tenant or to the platform. A tenant sees its own
  hosts and platform hosts, and on a shared platform host only its own
  placements and what they hold ([Operators](operators.md#looking)).
- **Operator keys** belong to no tenant and see every tenant. They are made
  only by `luxd admin create-operator-key`, never through the API. Treat one
  like root.

## Keys and sign-in

- API keys are stored hashed. Personal keys (`lux_u_…`) follow the person's
  membership role in their tenant; service-account keys (`lux_s_…`) follow
  that account's role. Read-only keys only read. Expired/revoked keys,
  disabled principals and removed memberships fail authentication.
  Personal keys have a concurrent-safe 5-active cap per person per tenant;
  service accounts have 2. Secrets are shown once, on creation.
- Roles are **viewer < member < admin < owner**. Viewers read everything
  (including artifacts, diffs, snapshots, costs and events), never terminal
  or input. Members submit Runs and act on their own Runs and those of
  service accounts they own. Admins act on every Run and manage members up
  to Admin, service accounts, tenant servers, pools and hosts. Owners also
  manage Owners. Only operators may remove or demote the last owner.
  Run mutations refuse another member with `403 not_run_owner`, naming
  the owner. Every role sees every Run; ownership is not a read boundary.
- **Standalone servers** may be created by members, including member service
  accounts; viewers and read-only keys cannot create or mutate them. While
  unattached, a server belongs to its original creator principal: the person,
  service account, or that account's owning member. Creation attribution is
  the existing creator email or key ID, resolved through tenant-scoped users,
  retained key records (including revoked keys), and service-account ownership.
  Key rotation and account renaming do not change ownership; names, labels,
  and hostnames cannot claim a server. Unresolvable legacy or platform-key
  creators remain admin-only. Admins and operators may manage every tenant
  server. While attached, only the current Run's authorization applies: creator
  status grants no additional control. Attach checks both the authoritative
  source server and the destination Run. Detach, sleep and re-attach preserve
  original creation attribution, never transferring it to the detacher.
- Removing a member immediately revokes their tenant keys, leaves their
  Runs running and attributed, and leaves their service accounts ownerless.
  Personal keys are visible only to self/admins; account keys to their
  owner/admins. Admins revoke a member's key
  (`DELETE /v1/members/{user}/keys/{key}`, event `key.revoked` by them);
  an owner's keys only owners and operators. Service accounts cannot sign in or have the Owner role.
- **Changing your own membership.** The server applies the same rules to
  you as to anyone (`checkOwnerChange`): an owner may demote themselves
  while another owner exists (the last owner gets 409 `last_owner`);
  operators may change or remove any membership, their own included, and
  keep operator access after. Only operators add members directly
  (`POST /v1/members`), and only operators use its `upsert` (set the role
  of someone already a member, `member.role_changed`), which is how a
  tenant's owner is assigned without an invitation.
- **Renaming, deleting and converting service accounts** is for admins
  and owners (403 for anyone else, read-only keys included). A rename is
  validated as a create and renames the account's active keys named
  exactly as the old name. Delete is refused (409 `has_runs`) while the
  account owns Runs, so attribution is never lost; its keys are revoked at
  once and their rows kept, revoked, as removing a member keeps a member's.
  **Convert** (`POST /v1/service-accounts/{sa}/convert {email}`) makes the
  account's active keys a member's personal keys, the same secrets, and
  moves its Runs to them, in one transaction under the tenant's members
  lock (a concurrent second convert finds no account). **Escalation
  guard:** a converted key acts with the member's role, not the
  account's, so a caller may convert only to a member whose role is at
  most their own: an admin to viewers, members and admins; to an owner,
  only owners and operators. Read-only keys stay read-only and expiries
  are kept; the member's 5-key cap holds (409 `key_limit`, nothing moved).
- The console signs in with a key, kept in browser-tab session storage, or
  through Cloudflare Access. luxd verifies signatures, audience and expiry.
  Only configured operator emails are operators; everyone else needs a
  membership in the selected tenant. Without one the API returns
  `403 not_a_member`; `/v1/me` and pending invitations remain available.
  `?tenant=` or `Lux-Tenant` selects a membership. An operator who is also
  a member of a tenant keeps full operator capability there, `?tenant=`
  included: the membership's role never narrows an operator (it only
  attributes the Runs they submit). Service tokens (no email)
  are refused. Access cookies authorize writes only with same-origin
  `Sec-Fetch-Site`, preventing cross-site forms from acting with them.
- Invitations require Access mode (`409 invitations_unavailable` in key
  mode). Links bind one verified email to one role and tenant for 14 days.
  A different email cannot inspect details or accept. No emails are sent.
  Expired invitations renew with the same code; accepted/revoked codes
  cannot be used again. Concurrent acceptance creates one membership.
- **Streams from a browser** (exec, attach, ports) authenticate with the
  Access cookie or a **stream ticket**: `POST /v1/runs/{id}/tickets`, made
  with the caller's own key, good once for 60 seconds, for one Run and one
  kind (exec or preview), stored hashed, and dead with the key that made
  it. Every stream upgrade from a page is refused (403 `bad_origin`)
  unless its `Origin` is luxd's `public_url`, the request's own host, or
  one in `console.allowed_origins`, so another site cannot open a shell
  with a visitor's cookie. Clients that send no `Origin` (the CLI) are not
  affected.
- **A terminal is the Run's secrets.** A shell in a Run sees the
  workload's environment, `env` secrets included: whoever may open one
  (`member` or above and ownership of the Run, or an admin) may read them.

## Workloads

- **Containers** run under rootful Podman with `--userns=auto`: each has
  its own user namespace, and container root is not host root. No
  privileged containers; nested containers are opt-in and still
  unprivileged ([Concepts](concepts.md)).
- **Egress** is denied by default per Run, with nftables and a DNS stub;
  the spec allows hosts ([The RunSpec](runspec.md)).
- **Secrets** are never stored: luxd holds their values in memory from the
  submit or resume that supplied them, runners get them over their
  WebSocket, and output is redacted before it is written anywhere
  ([Concepts](concepts.md)). A workload can still write a secret into its
  state volumes; that is its choice, and is snapshotted like anything else.
- **Credentials a workload uses but must not hold** go through a service
  (`workload.services`): lux-shim adds them to each request it forwards
  from a socket in the container. The values are only in the shim's
  memory, and the shim is not dumpable, so even container root (without
  `CAP_SYS_PTRACE`, which no Run has) can't read them from `/proc`. The
  workload does choose the request path, so point a service only at an
  API with no endpoint that reflects request headers. Git credentials
  never enter the container at all: the runner clones and pushes. Git
  mirrors on a host are per tenant: one tenant never clones from another's.
- **Operator registry tokens** (luxd's optional `[registry]`): luxd mints
  a pull-only token per granted owner through the configured provider,
  which promises that an owner's token can pull `<host>/<owner>/...` and
  nothing else. With ECR, the only provider today, luxd assumes the
  operator's pull role with the session tag `lux-owner=<owner>`, and the
  role's policy limits each token to pulling `<owner>/*` repositories: no
  push, no other owner's images, no other ECR or AWS action (two explicit
  denies, so a repository policy cannot widen it). luxd holds tokens in
  memory only (handed out while 30 minutes or more of validity remain),
  attaches them to an assignment when it is sent, only for the owners the
  Run's spec names in the registry, keyed `<host>/<owner>`, and never
  writes them to Postgres, an event, a log line or an error. On the host a
  token is in lux-runner's memory with the assignment, for the life of the
  placement, and in a 0600 `auth.json` only while the runner pulls or
  pushes; never in the container. Revoking a grant stops new tokens; an
  issued one stays valid until it expires (with ECR, at most the session's
  hour, or ECR's 12 hours). A tenant receives tokens only for the owners
  its grants list, but a token is not tenant scoped: every tenant granted
  an owner can pull all of that owner's repositories
  ([operations](operations.md#registry-pull-credentials)). A Run's stop or
  input waits (at most 10 seconds) behind its own assignment while that
  waits for a token; another tenant's Runs never wait for it.

## Previews

Servers can be reached at `https://<hostname>`, a name under the preview
domain ([Operators](operators.md#previews)). What protects that:

- **Always authenticated.** Cloudflare Access with the preview
  application's own AUD, where luxd also verifies the token and checks
  the verified Access user is an operator, a member of the server's tenant
  (any role), **or the server belongs to the configured default tenant**.
  Previews need no lux membership: external testers keep default-tenant
  previews. Other tenants require membership. This gate does not go through
  the API's `not_a_member` lookup. Or, in ticket mode, a `__Host-` cookie (host-only, `Secure`,
  `HttpOnly`, `SameSite=Lax`) that luxd signs (HMAC-SHA256 with a key it
  keeps in its database), bound to one server, and gives out only for a
  preview ticket of that very server (or of the Run it is attached to),
  minted by someone of its tenant. A cookie made from an API key lasts 12 hours and stops working
  (within a minute) when the key is revoked; one made by a person signed
  in through Cloudflare Access lasts 1 hour, since luxd cannot see the
  Access policy change without a fresh token. The console hands a preview
  ticket only to a host under luxd's own preview domain, with its scheme
  and port: a `/preview-auth` link to any other host is refused before a
  ticket is minted, so it cannot leak one.
- **Only people signed in wake a server.** An unauthenticated request (a
  chat app unfurling a link, a crawler) is sent to sign in or gets 401; it
  never emits `server.wake_requested` ([waking on
  request](concepts.md#waking-on-request)).
- **Hostnames are first come, first served** across tenants: a tenant can
  take a name another would want (`web.<domain>`), never one in use.
  Names are lower case DNS labels under the preview domain only.
- **Over http** (allowed only under `localhost`, for a local demo:
  [operators](operators.md#previews)): the cookie cannot be `Secure` or
  `__Host-`; it stays host-only and `HttpOnly`, and never leaves the
  machine.
- **A listener of its own** (`preview.listen`) that only proxies: it never
  serves `/v1`, `/runner` or the console, so preview content never shares
  an origin with them.
- **Header hygiene.** Toward the container, luxd removes
  `Cf-Access-Jwt-Assertion`, any `Authorization: Bearer lux…`, and the
  `CF_Authorization` and `__Host-lux_preview` cookies; it sets
  `X-Forwarded-Host`, `X-Forwarded-Proto: https` and `X-Lux-User` (the
  person's email or the key's name). From the container, every
  `Set-Cookie` loses its `Domain`, so a preview can set cookies for its
  own host only.
- **A server's `inject`** ([runspec](runspec.md#what-inject-touches)) is
  HTML its tenant chose, added to its own preview's pages and no other's;
  it runs with the page's origin, as the page's own code does. It is not
  secret, and lux leaves the page's `Content-Security-Policy` as it is.
- **What is left:** preview hosts are subdomains of the preview domain, so
  they are same-site with anything else under its registrable domain, and
  browsers send them cookies other apps set with `Domain=` that parent.
  Serve previews from a domain of their own, or make sure the apps beside
  them use host-only cookies. Preview content is the Run's code: treat a
  preview like any page its author wrote.

## Deployment

- **Reach luxd privately**: its API and console carry every tenant's data.
  Put it behind a tunnel, a VPN or Cloudflare Access; lux has no public
  URLs of its own (port forwarding goes through the CLI), except previews,
  when configured, which are always authenticated (above).
- **Secrets in configuration** (the database password, the S3 secret key)
  are best set in the environment. A configuration file holding them
  should be readable by luxd only (`chmod 600`); luxd warns when others can
  read it ([Operations](operations.md#configuration)).
- **Only luxd holds S3 credentials.** Runners upload and download through
  luxd and presigned URLs.
- **The managed image registry's credential** (`[images]`,
  [published images](runspec.md#published-images)) is luxd's and the
  runners': read by luxd from a file or its environment for each
  assignment that needs it, sent to that assignment's runner only, and
  never stored, logged, or given to a shim, a build or a container. The
  runner offers it to Podman only for the Run's tenant's repository and
  the shared ones; a tenant names neither another tenant's repository nor
  where its build is pushed. A shared repository is every tenant's to
  pull, whole: share only one meant for all of them. With `auth = "ecr"`,
  luxd gets ECR tokens with its own AWS credentials; a token carries
  every permission of that IAM principal, so give it only the managed
  root and the shared repositories.
- **Runners** authenticate with host tokens (`luxd admin
  create-host-token`), scoped to a tenant or the platform and a pool.
