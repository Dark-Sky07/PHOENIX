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

The initial agent persists and acknowledges its desired fleet document.  It
never executes a command from that document.  Connector providers are expected
to consume the validated endpoint contract through a dedicated agent adapter;
this prevents a template editor from gaining arbitrary code execution on a
node.  Existing Xray Tunnel/WireGuard/AmneziaWG inbounds are unrelated and
remain unchanged.

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
* `GET/POST /enrollments`, `POST /enrollments/:id/rotate`, and
  `POST /enrollments/:id/disable` manage the agent lifecycle.  Create and
  rotate responses contain the plaintext bootstrap/agent credential exactly
  once; later reads only expose `hasCredential`.
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

The agent reports an acknowledgement for a revision it has durably staged.  A
failed validation or persistence attempt is reported as a bounded diagnostic
message.  The control plane treats agent reports as health evidence, not as a
source of billing or traffic authority.

## Managed tunnel contract

A managed tunnel has a provider identifier, a public hostname, a local target
host/port and protocol (`http`, `https`, `tcp` or `udp`).  The target is a
local/private address because the connector runs on the node; public targets
are rejected to avoid turning the agent into an SSRF relay.  `providerConfig`
is encrypted with the panel secret using distinct authenticated-data purpose
`managed-tunnel-provider-v1`, never appears in operator read APIs, and is sent
only in an assigned agent's TLS response.

No tunnel starts on upgrade.  A tunnel is not supplied to an agent unless it is
enabled, referenced by the revision assigned to that agent, and the agent has
checked in successfully.

## Rollback and failure handling

A deployment is an explicit desired-revision update.  Operators can first
deploy to a canary enrollment set, inspect each assignment's `appliedRevision`,
`status`, `lastError`, and timestamps, and then target the remaining set.
Rollback selects an already immutable prior revision and creates no mutation of
revision history.  Disabled enrollments receive no desired state.  Agents keep
the last durably staged configuration during control-plane outages; they do
not erase or mutate legacy node configuration.
