# Phase C fleet control-plane design

Phase C is an opt-in control plane for PHOENIX nodes.  It deliberately does
not replace the existing `Node` HTTP-polling transport: a node without an
agent continues to use its configured panel URL, API token, inbound selection,
traffic accounting, and heartbeat exactly as it did before this release.

## Goals and non-goals

* An agent can reach the central panel from a node that has no inbound
  management address.  The central panel never has to dial the agent.
* Enrollment and ongoing credentials are one-time or rotating, are stored only
  as hashes by the panel, and are not exposed by list/read APIs.
* Fleet configuration is revisioned and immutable after publication.  An
  operator selects a revision and an explicit target set for each rollout;
  creating a revision never changes a node.
* A managed tunnel describes a provider-neutral *endpoint contract*.  Its
  provider credential is encrypted at rest and is delivered only to the
  assigned authenticated agent over TLS.  The agent protocol has no shell or
  arbitrary-command field.
* The first Phase C transport is a bounded outbound polling protocol.  It is a
  safe foundation for future persistent transports, but does not require a
  public agent listener, SSH access, or a particular tunnel vendor.

The agent persists and acknowledges its desired fleet document. It never
executes a command from that document. Connector execution is available only
through a built-in, locally configured adapter: the `cloudflared` adapter runs
a locally installed executable selected with the agent's `--cloudflared-path`,
not with a path or command supplied by the panel. Other providers remain
contract-only until PHOENIX ships a dedicated adapter. This prevents a template
editor from gaining arbitrary code execution on a node. Existing Xray
Tunnel/WireGuard/AmneziaWG inbounds are unrelated and remain unchanged.

## Data model and migration contract

All Phase C tables are new.  `AutoMigrate` creates them independently of the
existing `nodes` table, so an existing installation has no new required
settings and no changed node behavior.

| Table | Purpose |
| --- | --- |
| `fleet_templates` | Template identity and current revision metadata. |
| `fleet_template_revisions` | Immutable, canonical JSON documents keyed by template and revision. |
| `fleet_agent_enrollments` | Agent identity, hashed bootstrap/rotating credentials, health and optional legacy node link. |
| `fleet_assignments` | Explicit agent-to-template assignment, desired/applied revision and rollout state. |
| `fleet_pools` / `fleet_pool_members` | Reusable regional, canary, or maintenance cohorts; pools may bind a template to prevent a mistaken deploy. |
| `managed_tunnels` | Validated public endpoint/localhost target contract and encrypted provider credential. |
| `fleet_agent_tunnel_status` | Bounded agent-reported runtime observation for each assigned tunnel; no logs, command lines, or credentials. |

The optional `node_id` on an enrollment is a compatibility bridge only.  It
allows an operator to associate an outbound-only agent with an existing legacy
node record; it does not change `runtime.Remote`, snapshot provisioning,
traffic synchronization, or the legacy heartbeat job.

## Template document schema

A template revision stores a canonical document of this shape:

```json
{
  "schemaVersion": 1,
  "labels": {"region": "de-fra", "tier": "edge"},
  "node": {"inboundSyncMode": "all", "inboundTags": []},
  "managedTunnelIds": [12, 18]
}
```

`schemaVersion` is currently exactly `1`.  Labels are bounded strings.
`inboundSyncMode` is `all` or `selected`; selected mode requires unique
non-empty tags.  Tunnel IDs are positive and unique.  Unknown fields are
rejected, rather than being carried to an agent silently.  A SHA-256 of the
canonical JSON is stored with every revision.

The template payload expresses desired configuration only.  It cannot contain
a program, command, URL fetch instruction, file path, environment variable, or
shell interpolation.

## Operator API contract

Authenticated administrator endpoints are mounted below `/panel/api/fleet`:

* `GET /templates`, `POST /templates`, `GET /templates/:id`
* `POST /templates/:id/revisions` creates an immutable next revision.
* `GET/POST /pools`, `POST /pools/:id`, and `POST /pools/:id/delete` manage
  reusable enrollment cohorts. A pool may bind one template; deployment then
  rejects a different template. `deploy` accepts either a pool ID or an
  explicit enrollment ID list, never an implicit fleet-wide target.
* `POST /templates/:id/deploy` changes the desired revision only for the
  requested enrollment IDs or one explicit pool. This is the staged rollout
  mechanism.
* `POST /templates/:id/rollback` moves explicitly requested enrolled agents to
  a prior revision.
* `GET/POST /enrollments`, `POST /enrollments/:id/installer`,
  `POST /enrollments/:id/update`, `POST /enrollments/:id/rotate`, and
  `POST /enrollments/:id/disable` manage
  the agent lifecycle. Create and rotate responses contain the plaintext
  bootstrap/agent credential exactly once; later reads only expose
  `hasCredential`. The installer endpoint reissues a pending enrollment's
  one-time bootstrap token and returns a reviewed **local** bootstrap script;
  it does not dial the node, retain SSH credentials, or execute a remote
  command.
* `GET/POST /tunnels`, `POST /tunnels/:id`, and
  `POST /tunnels/:id/delete` manage managed endpoint contracts.  Credentials
  have write-only semantics.

Fleet mutation endpoints require a recent interactive superadmin session.  A
copied API bearer token cannot create a node bootstrap credential or expose a
provider tunnel credential.

## Agent protocol

The agent is invoked as `phoenix agent` and keeps its state file mode `0600`.
It first calls the enrollment URL with its bootstrap token and a stable,
random agent ID.  The panel atomically consumes the bootstrap token and returns
an agent credential.  The panel stores SHA-256 hashes, not either plaintext
credential.

The agent subsequently makes outbound HTTPS requests to:

* `POST /panel/agent/v1/enroll`
* `POST /panel/agent/v1/checkin`

`Authorization: Bearer <agent credential>` authenticates check-ins.  Check-in
bodies and responses are size-bounded.  The response includes at most the
assignment's desired template document plus only the managed tunnels referenced
by that document.  It can include a replacement credential; the agent writes
that replacement atomically before using it on the next poll.  Credentials are
rotated by an operator or periodically by the panel and the previous hash is
not retained.

The agent reports an acknowledgement only after it has durably staged the
revision and reconciled its supported managed-tunnel adapters. A failed
validation, persistence, or connector start keeps the revision unacknowledged,
persists it for a later reconciliation retry, and is reported as a bounded
diagnostic message. Check-ins also carry bounded per-tunnel runtime status
(`running`, `stopping`, `stopped`, `error`, or `unsupported`), which the panel
persists as operational telemetry. During a rollout, the panel accepts reports
for both the current desired and last applied revision while the agent
reconciles the change, but only displays telemetry for the currently enabled
desired tunnel contract.
Historical status rows remain diagnostic evidence rather than appearing as
current health. The control plane treats agent reports as health evidence, not
as a source of billing or traffic authority.

## Node installation and updates

A node that has no PHOENIX agent cannot be safely installed by an outbound-only
control plane: accepting a panel-originated shell command would turn an
unmanaged host into remote-code-execution surface. For a pending enrollment,
an operator may instead request `POST /enrollments/:id/installer` after recent
superadmin authentication. The panel invalidates the prior unused bootstrap
value and returns a short-lived script for the operator to review and run
**locally as root**.

The script only downloads the latest official `Dark-Sky07/PHOENIX` release,
requires a numeric `vX.Y.Z-phoenix` tag, downloads the matching published
SHA-256 sidecar, and refuses extraction unless that checksum verifies. It
installs only `/usr/local/bin/x-ui`, performs one agent enrollment with
`phoenix agent --once`, and writes a hardened systemd unit that contains no
bootstrap token. It has no user-selectable repository, URL, executable,
arguments, shell fragment, or HTTP override. Its fixed CPU detection passes a
local architecture enum only to preserve the reviewed official ARM artifact
selection; the panel cannot choose it. A self-signed panel can be pinned with
the optional base64 certificate SHA-256 supplied to the installer request.

For a previously enrolled Fleet node, a recent superadmin can explicitly
request `POST /enrollments/:id/update`. This is deliberately a one-agent
canary operation; the operator repeats it only for the next reviewed cohort.
When queued, the panel resolves the latest official numeric release **once**
and records its release tag, the enrolled agent's declared architecture, and
the corresponding official archive SHA-256. The agent receives only that
pinned identity/digest plus the enum `official-release-v1`; it constructs the
fixed GitHub asset path itself and refuses any URL, binary, path, argument,
environment, command, or source supplied by the panel.

The agent checks both the published sidecar and downloaded archive against the
panel-pinned digest, extracts only `x-ui/x-ui`, and atomically replaces only
its installer-owned `/usr/local/bin/x-ui`. It persists bounded `accepted`,
`installed`, or 512-byte `failed` status. A request accepts acknowledgement for
at most 30 minutes; a later agent check-in marks an unacknowledged request as
failed. After success, the agent exits so systemd restarts the new binary to
acknowledge it. A download, checksum, layout, or write failure leaves the
running binary untouched; a disabled enrollment
cancels an outstanding request. There is no arbitrary rollback artifact: a
new explicit request is another pinned official-release rollout, while the
atomic replacement provides failure rollback before a version is installed.

All queueing actions are covered by the panel's standard redacted mutation
audit trail under the `fleet` module; it records actor, target enrollment, and
HTTP status. Pinned metadata remains visible in enrollment state without ever
recording agent credentials.

The existing **Nodes → Update Panel** action remains the backward-compatible
update path for legacy direct nodes: it invokes the already-authenticated
remote panel updater. It never makes Fleet agents execute an operator-supplied
command. Fleet bootstrap artifacts and legacy panel updates therefore remain
limited to PHOENIX's official release/checksum pipeline.

## Managed tunnel contract

A managed tunnel has a provider identifier, a public hostname, a local target
host/port and protocol (`http`, `https`, `tcp` or `udp`).  The target is a
local/private address because the connector runs on the node; public targets
are rejected to avoid turning the agent into an SSRF relay.  `providerConfig`
is encrypted with the panel secret using distinct authenticated-data purpose
`managed-tunnel-provider-v1`, never appears in operator read APIs, and is sent
only in an assigned agent's TLS response.

No tunnel starts on upgrade. A tunnel is not supplied to an agent unless it is
enabled, referenced by the revision assigned to that agent, and the agent has
checked in successfully.

The shipped `cloudflared` adapter supports `http`, `https`, and `tcp` targets.
It requires a locally installed `cloudflared` binary passed by absolute path to
`phoenix agent --cloudflared-path`; the panel cannot choose that path. Its
provider configuration is a JSON object with `tunnelId` and a Cloudflare
credential JSON object under `credentials`. The agent writes the generated
YAML and credential JSON only below its owner-only tunnel directory, invokes
`cloudflared --no-autoupdate --config … tunnel run` without a shell, and
supervises/reports the child process. An IPv6 local target is rendered with a
bracketed host/port. A changed contract stops the old connector before a later
heartbeat starts a replacement, so two processes do not overlap for one tunnel.
On endpoint withdrawal, disablement, or a terminal disabled-enrollment response,
the agent stops the connector and removes that tunnel's generated credential and
configuration directory. A normal agent process restart preserves owner-only
generated files and rehydrates its protected state before it reports health.
`udp` and unimplemented providers stay rejected/`unsupported` rather than being
guessed or run with arbitrary flags.

## Rollback and failure handling

A deployment is an explicit desired-revision update.  Operators can first
deploy to a canary enrollment set, inspect each assignment's `appliedRevision`,
`status`, `lastError`, and timestamps, and then target the remaining set.
Rollback selects an already immutable prior revision and creates no mutation of
revision history. Disabled enrollments receive a terminal `disabled` check-in
response when they are reachable; the agent stops managed connectors but keeps
its protected last state so re-enabling can resume the explicit assignment. An
offline node cannot be stopped through an outbound-only transport, so operators
must stop its local service if immediate isolation is required. Agents keep the
last durably staged configuration during control-plane outages; they do not
erase or mutate legacy node configuration.
