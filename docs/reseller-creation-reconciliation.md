# Interrupted single reseller client creation

Single-client reseller creation and public signup record a durable intent before creating the
client. Completion removes the intent in the same database transaction as the
ownership mapping and sale. Reseller bulk creation and import use a batch
intent before mutations; they remove it after an entirely successful batch.
Partial batches and uncertain errors keep the intent and require review. If the panel stops mid-operation or compensation
fails, an intent may remain in `reseller_creation_intents`. While it remains,
reseller mutations and participating quota checks fail closed for that reseller.

An administrator must investigate the intent's email, subscription identity,
requested quota, inbound IDs and declared price against client records,
inbound settings, traffic, sale/mapping records, runtime state and backups.
Do not blindly delete the marker or replay the request: effects may be partial
and runtime traffic may not be fully accounted for. Reconcile and document the
actual state before removing any marker. An absent marker after a database
error does not itself prove whether an operation succeeded; verify the sale,
client and mappings. This is a safety barrier, not an atomic creation protocol.
Batch metadata contains subscription IDs, which are sensitive; restrict database and backup access.
The batch marker is not removed atomically with all client/runtime changes, so a
crash after successful changes can still require manual reconciliation.

## Ambiguous client traffic identities

Quota calculations, renewal and reseller traffic archives reject multiple local
traffic rows whose emails are equal after trimming and lowercasing, rather than
charging the same global counter more than once or silently dropping usage.
Likewise, archiving rejects ambiguous client records. These errors require
administrator investigation. Back up the database first; compare the exact
emails, client ownership, local and master counters, prior archives and runtime
state. Do not simply delete or merge counters without determining their origin
and preserving consumed bytes. Archives produced before this guard may already
contain an inflated snapshot and require evidence-based reconciliation.

## Xray poll identity capture

The Xray poll now holds the participating reseller-allocation lock from before
reading runtime counters until their database identity snapshot is captured. A
capture error retains the batch and lock and retries capture on a later tick;
client allocation/mutation requests may wait while the database is unavailable.
This closes the identity-change race only for paths that take that lock. It
cannot fence a lost PostgreSQL lock session, cover unrelated runtime operations,
or recover traffic lost if the process crashes before its journal is durable.
Never assume this mechanism accounts for unpolled usage at deletion.

The Xray traffic baseline is shared across service instances in one panel
process. Because the panel creates its managed core rather than attaching to an
existing core, a new managed process is counted from zero, including its first
poll; replacing a job does not recount the same core. This does not recover
unpolled bytes when the core stops, or bridge the crash window before a batch is
journaled, and does not coordinate independently managed external cores.

## Unverified final traffic and reseller removal

Reseller-session client deletion, depleted-client deletion, inbound detach and
quota reduction are temporarily blocked. Persisted counters alone cannot prove
that the runtime has no unpolled traffic. This deliberately keeps the client
mapping and its allocation rather than inventing usage or releasing capacity.
Resellers should contact an administrator. Admin paths remain available and
are **not** an automated verification or fencing mechanism. Before an admin
removes a client or mapping or reduces quota, they must stop further traffic,
verify final usage against all runtimes, nodes, journals and payment/backup
records, and document any manual reconciliation. Where final usage cannot be
verified, do not proceed. Admin actions, external processes and database edits
can bypass this reseller-session restriction; it does not guarantee lossless
accounting under crashes or concurrent external runtime traffic.

An in-process panel scheduler replacement reuses the same Xray traffic job so
an uncommitted batch or retained capture lock is not abandoned during a soft
restart. This does not make the in-memory batch crash durable or fence a lost
PostgreSQL lock connection; journal and runtime reconciliation are still needed.

On orderly panel shutdown, the Xray collector retries its pending batch and
attempts one final poll before stopping the core. Failures are logged and
returned by the stop operation; a pending batch remains available for an
in-process retry. Traffic can continue between the last poll and core shutdown,
and a crash can bypass this step, so it is not proof of final usage.

The dashboard's explicit Xray stop and restart controls now attempt the same
flush first and refuse the action if that attempt fails. Other restart paths,
including automatic recovery, updates and externally managed runtimes, are not
covered. A successful flush is still not proof of quiescence: connections may
transfer bytes until the core stops.

Automatic renewal refuses a reseller client when an attached inbound's
configured traffic quota differs from the canonical client quota, or its client
data cannot be decoded. Inspect and reconcile the affected inbound and client
records before retrying; do not assume the persisted quota proves the runtime's
actual final usage. Renewal also avoids copying stale inbound fields over
existing canonical client records. This does not fence active connections,
external nodes, or bytes that have not yet been polled.

A journaled Xray delta for a reseller requires one unambiguous client, traffic
counter and owner both at capture and application time. Missing or duplicate
identities stop replay and retain the journal for manual reconciliation even
when the ambiguous identity did not change. An operator must investigate the
counter and owner records before resuming collection; this does not recover
traffic lost before a journal was written.

Xray polls reject duplicate or negative traffic counters and malformed nil
entries before advancing any in-memory baseline, so a later valid poll can
retry that interval. Other, non-traffic statistics are ignored. This protects
against malformed responses while the same core remains available; it does
not make counters durable across a core crash or stop.

When disabling locally depleted clients, the panel now rejects inconsistent
inbound settings instead of silently leaving a linked client enabled there,
keeps large JSON integers intact, and requests an Xray restart if API setup
fails. Remote pushes can still fail independently and require operator checks;
a restart request is not proof that live connections have stopped.

Negative persisted client, global or archive counters are invalid, not zero
usage. Reseller quota calculations, renewal and archival now fail closed when
such records are encountered; client traffic writes also refuse to overwrite a
negative local counter. Investigate and restore the correct usage from reliable
records before resuming. These checks do not reconstruct missing traffic.

Global traffic pushes reject missing identities, duplicate normalized emails,
ambiguous local counter identities and negative stored source counters rather
than overwriting them with a possibly lower or guessed value. A legitimate
source must send one nonnegative snapshot per client; the affected records
need manual reconciliation if the local database is already ambiguous. This
does not establish ordering or authenticity of external snapshots.

Node sync now refuses a non-dirty snapshot that omits an inbound, client or
traffic row for a reseller client already attached to that node, or changes
its configured or traffic quota relative to the canonical record. The database
transaction is not committed; investigate the node and final traffic before
reconciling. This preserves known allocations but can pause sync for the entire
node, and is not proof of final usage: unpolled bytes, direct node changes,
non-participating writers and malformed or omitted identities remain risks.
The sync also refuses newly introduced node associations for mapped reseller
clients, including traffic-only reports, until manually reconciled.

Node traffic snapshots now reject negative or conflicting counters, duplicate
client rows within an inbound, and corrupt or ambiguous stored baselines for
reported clients before applying the snapshot. Delta additions saturate rather
than wrap. Rejected snapshots require investigation and can stall collection;
they do not establish a trustworthy final reading from the remote node.

For already-attached reseller clients, node sync also checks authorization and
renewal fields against the last accepted inbound settings and checks traffic
expiry/reset values against the local counter. Unexpected changes pause sync
for manual reconciliation. Node sync preserves the canonical reseller client
record rather than overwriting its credentials or renewal settings, and rolls
back on link synchronization errors. These checks cannot undo a change already
applied directly on a node or establish its final traffic.

When a node reports an already-attached reseller client but has no recorded
per-node baseline, the sync now stops instead of assuming that the node's
current counters were already counted. An administrator must investigate and
restore a baseline only from trustworthy records; neither zero nor the current
node reading is automatically asserted to be correct. New deployments and
migrations may therefore need reconciliation before their first node sync.

A missing, negative or ambiguous per-node baseline for an attached reseller
client also blocks reseller quota decisions and renewal reservations, including
unlimited-reseller allocation, rather than displaying the known counters as a
complete account. Restoring a baseline permits these checks again; this is not
a health check and does not guarantee that a node's latest traffic arrived.

Node traffic collection now holds the participating allocation lock from before
requesting a snapshot through its database merge, so a participating ownership
or allocation mutation cannot race a successful in-flight snapshot. This
serializes node snapshot requests and can delay allocation while a node is slow.
It does not fence the remote node, make a failed poll durable, cover external
writers or guarantee final traffic. A failed snapshot can still require review.

A node counter that decreases for an attached reseller client may have reset
between polls, losing traffic. The panel rejects that snapshot and records a
`reseller_node_counter_incidents` marker with the previous and observed values.
The marker blocks further sync for the affected node and quota/renewal checks
for the affected reseller even if the counter later rises. Back up the database,
stop traffic, investigate all runtimes and logs, and reconcile actual usage and
baseline from evidence before an administrator removes the marker. Do not
interpret the difference or the new counter as the missing usage. If writing the
marker itself fails, the error is logged but no durable barrier is guaranteed;
crashes before detection and resets that exceed the old reading are not detected.

The incident is associated with the reseller mapping by normalized email, not
only by the current node attachment: removing an attachment does not clear the
owner's quota barrier, and the node stays blocked even if the client disappears
from later snapshots. Deleting or transferring the ownership mapping directly
can still bypass that association and requires manual audit/reconciliation.

The panel rejects explicit reseller-client unassignment and reseller deletion
while a mapped client has an unresolved node-counter incident, including mixed
unassignment batches. After evidence-based reconciliation an administrator can
remove the marker and retry. Other administrative client-deletion paths and
direct database writes are not automatically fenced by this check; do not use
them to bypass unresolved accounting.
