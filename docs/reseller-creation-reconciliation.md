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

Known counter incidents now also block client deletion, scoped deletion,
detachment and depleted-client cleanup before their normal runtime changes;
whole admin bulk batches fail closed if they contain an affected identity.
The archive transaction rechecks the marker. Non-participating processes,
direct database operations, and a newly detected incident racing a service
caller that does not hold the allocation lock are not fenced by preflight.

Deleting an inbound is also preflighted against unresolved counter incidents
using its linked clients, traffic records, legacy settings identities, and (for
a remote inbound) incidents on that node. Affected inbounds are skipped by
bulk deletion. This preflight does not fence external mutations or a newly
created incident racing a caller that does not hold the allocation lock.

The local Xray collector now rejects a decrease in an observed user counter
without advancing any counters in that poll. It records an incident using node
ID zero and a `direction` of `uplink` or `downlink`; only that direction's
previous/observed pair is evidence, and the other pair is not a measurement.
Collection and affected reseller quota/removal are blocked until manual review.
This protects a detected reset, not crashes before detection, unpolled bytes,
or resets whose new counter already exceeds the last reading. If the incident
cannot be persisted, only the error is reported; a crash can lose the warning.

If writing a detected local Xray counter incident fails, the panel retains the
original observation in memory and retries its persistence before polling again,
even when the managed core changes. A process crash can still lose this pending
observation. During a failed write, this panel process blocks participating
quota, client removal and inbound deletion checks; other processes are not
fenced, so this is not a durable or cross-process safety scheme.

When persisting a detected remote-node counter incident fails, the first
observation is retained in panel memory and retried before the next snapshot
for that node. Participating quota and removal checks in the same process are
blocked while it is pending. If the node remains offline this retry may wait
until it syncs again; a panel crash loses the pending observation, and other
processes are not fenced until the incident is durable.

If the database write for a detected node or local Xray counter incident fails,
the panel also attempts to fsync a fallback evidence file under
`counter-incident-fallback` inside the configured database folder. The presence
of any file there blocks participating quota/removal checks and subsequent
traffic collection until manual review, even after a panel process restart.
Files are not automatically cleared when the DB recovers: back up the folder,
inspect the evidence and DB, reconcile final usage, and only then remove the
fallback and any corresponding database marker. The other counter direction in
a local Xray record is not evidence. This is a local-filesystem safeguard, not
cross-host fencing; a crash before file durability, simultaneous DB and disk
failure, and Windows directory durability remain limitations. Database folder
permissions and backups must protect the recorded emails and counter values.

For newly detected incidents the panel now attempts the fsynced fallback before
writing the database marker, rather than waiting for a DB error. Both copies,
when present, stay blocked until reviewed and manually reconciled. A detected
incident can therefore stop all participating local/node collection even when
the DB write succeeds. If fallback storage fails but the DB marker succeeds,
the DB barrier remains; neither storage can cover a crash before the first
successful durable write or fence a separate host with an unshared folder.

An isolated-process regression test now checks that a durable fallback written
by one process remains a barrier in a freshly launched process sharing the same
database folder, without depending on a database incident. This does not cover
separate machines with different folders or simulate every filesystem failure.

PostgreSQL CI additionally verifies that a persisted counter incident written
by one process blocks a second process using a different database folder. This
only applies after the database incident is committed; it does not fence another
host during a failed or interrupted write or recover missing traffic.

Before restarting a managed Xray process observed unexpectedly stopped, the
panel records a global `core-exited` incident (node ID zero, empty email). This
is an uncertainty marker, not a traffic measurement; no affected clients or
missing byte count are inferred. The marker and fallback block participating
quota/removal checks and local traffic collection until an administrator has
investigated the whole affected runtime epoch. A failed DB write prevents that
restart attempt and leaves a local barrier for retry; the fallback is local to
its folder. A process that failed to start can conservatively trigger the same
marker. This does not detect a crash of the panel itself, a core that disappears
before the panel observes it, planned restarts with traffic after the last poll,
or concurrent mutations outside the participating locks.

The watchdog now records an observed unexpected managed-core exit on its first
health check, before waiting for the second check used to trigger restart.
A failed incident write postpones the restart attempt and is retried. A nil
process at initial startup and an explicitly stopped process do not invent an
exit. This still leaves a window between the core stopping and the first
observation; concurrent external or non-participating writers are not fenced.

The managed Xray process now has an exit watcher that attempts to record the
same uncertainty incident immediately after the child is reaped, rather than
waiting for the periodic health check. Intentional stops and replaced cores are
not treated as crashes. If persistence fails, the existing in-memory barrier and
watchdog retry remain in effect. This reduces but cannot eliminate the gap
between exit and observation, and does not detect a crash of the panel itself.

After the managed child exit is observed, the watcher now sets a process-local
barrier before waiting on the Xray process lock, so participating quota and
removal checks cannot proceed merely because that lock is busy. A later manual
stop flag alone does not erase an already-unexpected exit. The scheduler gap
before the watcher runs, panel crashes, other hosts and a stop request racing
the child exit are still not fully fenced.

The managed child wait path now registers the local exit barrier before its
completion signal closes, so it does not depend on scheduling a second watcher
before participating quota checks see the observation. The callback is skipped
for known intentional stops, and the watcher persists the incident afterward.
There remains a gap before the operating system reaps the child, a stop/exit
race, and no fencing of other hosts before the incident is durably recorded.

A manual stop request that finds an unexpectedly stopped managed core now
records the uncertainty before setting the manual-stop flag. Restart also
rechecks for a core that died while configuration was built and refuses to
start a replacement when stopping the old core fails. This closes specific
late-stop and ignored-stop-error paths, but a signal/exit race and traffic after
an ordinary final poll remain unresolved.

The administrator dashboard now displays an incident summary when database
markers, local fallback files or in-process observations exist, and warns when
the summary cannot be checked. It exposes only counts/flags, not client
identities or traffic values. A clear summary is not proof of complete traffic;
it cannot see incidents not yet detected or fallback files on another host.
An absent or malformed incident summary is also treated as unavailable by the
administrator dashboard (including when an older backend returns no summary).
It is not interpreted as evidence that there are no incidents.

Global-traffic snapshot ingestion now takes the shared allocation lock before
entering the traffic writer and holds it through the snapshot transaction. This
serializes participating quota/ownership operations and other participating
pushes using the same database, including across cooperating panel processes.
It does not authenticate the source, order old snapshots, fence a lost lock
connection or recover bytes that a master has not reported.

Global snapshots match each master's previous row using the same trimmed,
case-insensitive client identity used for accounting. A single older spelling
variant is updated in place when counters do not decrease; duplicate normalized
rows or a negative previous counter are rejected. Existing duplicates require
evidence-based manual reconciliation.

### Unversioned upstream counter decreases

A lower global counter for a reseller-owned client is ambiguous: it could be a
legitimate reset, an out-of-order or replayed snapshot, or missing traffic.
The panel no longer guesses that it is a new window. It rejects the whole
snapshot without changing its baseline, records the first observed values and
claimed master GUID as a `node_id = -1`, `direction = global-decreased`
incident, and writes a local fallback file before the database marker. Quota,
participating removal and subsequent snapshot ingestion are blocked until an
administrator investigates. Legitimate upstream resets therefore require manual
reconciliation too; this is an intentional availability trade-off.

Keep the old counters, incident row and fallback file while comparing the
upstream's actual reset history, all runtimes, journals and backups. The master
GUID is claimed by the request and is not proof of source identity. Do not
invent missing bytes or clear markers merely because counters rise again.
After a documented reconciliation, remove both the database incident and its
local fallback, including on every affected host, following the usual incident
procedure. A failed DB write retains the first observation in process memory
and retries on a later push; if the process crashes before durable storage, or
both DB and fallback storage fail, other hosts cannot be fenced. Increases and
snapshots accepted before this protection may still be stale or misleading: the
protocol has neither source-bound authentication nor generation/sequence
numbers. Full ordering needs an authenticated, durable upstream protocol.

Xray polling now retains the participating allocation lock until the captured
identity snapshot and traffic batch have been written and synced to the local
journal. A journal-write failure keeps the batch and lock for retry instead of
allowing allocation or ownership changes before journal durability. This
reduces that concurrency window; it does not recover a crash during the poll
or before the journal is durable, account for unpolled bytes, or fence writers
that do not use the shared lock. A prolonged disk failure can block participating
allocation operations until storage is repaired or the process stops.

The Xray collector now holds that allocation lock beyond journal sync, until
its batch has been applied to the database and the journal cleanup succeeds.
Otherwise a quota check could see the old database counters despite a durable
but unapplied batch. Failed database application keeps the lock and batch for
retry; automatic renewals in this path use the already-held lock rather than
trying to reacquire it. An unresolved disk, database or reconciliation error
can therefore block participating allocation operations. This does not fence a
lost lock session, protect other hosts' independent journals, or eliminate the
crash window before the journal is durable.

### Interrupted local Xray poll

Before querying Xray, the collector now commits a database in-flight marker
while holding the shared allocation lock. The marker is global because client
identities and byte counts are not known yet. Participating allocation,
ownership and removal operations, quota checks, and automatic renewal without
the already-held lock fail closed while any marker exists. A successfully
journaled batch is replayed under the recovery lock and its marker is removed
only after the database application and durable journal removal. The dashboard
includes in-flight markers in its incident count; an active poll may briefly
appear and then disappear.

A marker without a corresponding journal may mean the panel crashed during
the poll, during identity capture, before journal sync, or after the journal
was removed but before marker deletion. It is not evidence of how many bytes
were lost. Back up the database and every host's journal directory; inspect the
marker ID, receipt, counters, runtime and operator records before any manual
reconciliation. Do not blindly delete a marker or receipt. In particular,
receipt and journal cleanup ordering protects against double application.
Journals are still local to each host: recovery must run on the host holding
the matching journal. The database marker can block participating processes
sharing that database but not a disconnected host, a lost advisory-lock
session, or non-participating writers. Traffic between polls, failures before
marker creation, panel/core crashes and traffic after a final poll are not
reconstructed or proven complete by this mechanism.

A local Xray journal now also blocks participating allocation and direct quota
checks even if it predates the database poll marker or its marker is absent.
Replay takes the recovery lock and removes the block only after successful
application and durable cleanup. Unexpected files (including incomplete
`.pending-*` writes) and journal-directory inspection failures fail closed;
retain and investigate them rather than deleting them blindly. The dashboard
includes local journal presence or inspection failure in its warning. This
covers only the configured local journal folder: an older, unmarked journal on
another host with an independent folder is not visible to this guard.

Recovery can automatically clear an interrupted poll marker when its matching
Xray receipt proves that the traffic transaction committed. It retains the
receipt because a journal for that batch could still exist on another host;
a later replay must not charge it twice. Markers without a matching receipt are
not cleared by this recovery and still require their journal or documented
manual investigation. Orphan receipts may accumulate until an operator can
confirm no copy of their corresponding journal can be replayed; do not remove
them merely to reduce table size.

Xray batch receipts are no longer automatically deleted after a local journal
is removed. That removal cannot prove that another host or backup lacks a copy;
retaining the receipt prevents that copy from charging the same committed UUID
again. This change is prospective: previously deleted receipts cannot be
reconstructed reliably without evidence, and restoring a database backup that
predates a receipt can still permit a duplicate replay. Retention grows the
receipt table for every committed batch, including empty polls; monitor database
size and back up receipts with the counters. Do not prune receipts unless every
possible journal copy and backup replay has been accounted for under a verified
retention policy.

### Ordered authenticated snapshots: implementation in progress

The experimental verified global-traffic path authenticates exact snapshot
bytes, registered sender and generation, and a sequence using HMAC-SHA256.
The receiver binds a full-scope API bearer-token ID to a pre-provisioned
`api-token:<token-id>` receiver row. The database enforces one receiver
sequence stream per registered sender; existing duplicate registrations must be
reconciled before migration because the unique index will fail closed. The
legacy global endpoint rejects writes claiming an already registered sender
while holding the allocation lock; provisioning must use that same lock. This
cannot authenticate an unregistered sender or prevent a full-access token from
using other administrative APIs. Its key is domain-separated from that
same token, not an independent credential: token compromise permits signatures
and all other operations allowed by the token. Use a distinct token per peer,
verified TLS and restricted administration. The receiver commits the
accounting write and accepted sequence in the same transaction. A request
cannot register a sender, choose a receiver row, or change its generation.

The scheduled sender uses this path only for nodes explicitly marked
`verified_global_push`, with pre-provisioned `node:<id>` sender state. It
captures under the participating allocation lock, reserves a durable sequence,
and requires HTTPS with ordinary certificate verification. It does not fall
back to the old endpoint after a verification, provisioning, transport or
receiver failure. Ordinary node edits preserve the mode. Unmarked nodes still
use the legacy endpoint; unregistered sender identities are not protected by
the registered-sender downgrade guard.
Verified mode is not automatically provisioned. The Nodes page provides an
experimental interactive superadmin setup dialog. Do not enable it until both peers, their identities, generations, sequence
state and token are configured and reconciled under a reviewed procedure.

The agreed rollout is **not complete**: inbound node snapshots remain legacy;
the dashboard and limited reseller guard described below do not cover all write
paths. Cross-process failover/integration testing remains necessary. These mechanisms
do not prove that a runtime observed or reported all traffic. The lock cannot
fence a lost PostgreSQL session or nonparticipating writers. Database rollback
can restore an old sequence and invalidate replay protection unless there is an
external monotonic anchor or explicit recovery procedure. Never automatically
accept a new generation or reset sequence state to clear an error. Back up
sequence state together with accounting data and reconcile restoration, key
rotation and generation changes; do not clear incident markers merely because
this experimental path exists.

Node global-traffic forwarding rejects an unreadable global overlay, negative
counters or ambiguous normalized local/global identities instead of sending a
partial or malformed snapshot. An error may leave a node with an older snapshot;
investigate it before relying on usage. This does not authenticate the legacy
path or prove snapshot freshness.

Experimental peer registration is available through interactive superadmin
sessions only: `POST /panel/api/nodes/trafficSnapshot/sender/:id` creates a
fresh sender generation without enabling verified mode, and
`POST /panel/api/nodes/trafficSnapshot/receiver` binds a full-scope API token
ID to that sender and generation. Both require `reconciled: true`; this is an
operator confirmation, **not** proof that accounting is complete or the other
peer is ready. The sender endpoint returns the sender GUID and generation for
secure out-of-band transfer to the receiver. Then use
`POST /panel/api/nodes/trafficSnapshot/sender/:id/activate` with a fresh manual
confirmation: it authenticates a nonce-bound receiver readiness response before
activating. Legacy pushes continue until activation and may fail once the
receiver blocks that registered legacy identity. Readiness is a point-in-time
check, not proof of accounting completeness or future availability. Existing
senders enabled by an older version are not automatically rechecked. Neither endpoint overwrites existing state or
resets a sequence; token rotation, generation changes, backup rollback, existing
usage and incidents require manual reconciliation. The Nodes page exposes
these registration operations and can reread an existing sender identity and
generation, and can activate after the readiness check. Unregistered peers still use legacy paths.

The administrator dashboard now warns about every registered node, including
disabled nodes and nodes with verified outgoing global pushes, because incoming
node snapshots do not yet have an authenticated ordered protocol. The warning
links to the existing node inventory conceptually (all registered nodes are
affected); it does not list credentials or client identities. If node status
cannot be read, the dashboard warns rather than treating it as clear. A zero
node count is not proof that all historical or unpolled traffic is complete.
The dashboard itself is visibility only. The scoped quota/renewal guard below
does not cover every write path, and an experimental verified outbound push is
not proof of safe incoming node accounting.

As an interim fail-closed measure, participating reseller traffic/client quota
checks and automatic renewal reservations reject accounts assigned a remote
inbound or owning a client attached to one. This applies to unlimited accounts
and to nodes with verified outbound pushes: incoming node snapshots are still
unverified. Normal quota/accounting mutation callers must hold the allocation
lock. The rule can block otherwise healthy resellers until an incoming protocol
and an evidence-based migration exist; it does not shut down the node. It is
not a complete perimeter: direct administrator/runtime/database writes,
unrepresented or lost associations, upstream global-only traffic and other
nonparticipating paths may bypass it. Existing totals can still be incomplete.

The experimental node-to-master snapshot endpoint
`POST /panel/api/inbounds/verifiedNodeSnapshot` requires a full bearer token and
an independently provisioned `node-snapshot:api-token:<id>` sender state. It
captures local database traffic without the global overlay, uses a distinct
node-snapshot key, and reserves its sequence while holding the allocation lock.
It rejects missing traffic, ambiguous identities, malformed settings, nested
nodes, and other-admin inbounds rather than signing a partial view. It does not
poll Xray or establish completeness of historical or unpolled usage. The interactive-superadmin API (not yet the UI) stages a node producer with
`POST /panel/api/nodes/nodeSnapshot/producer` (`tokenId`, `reconciled: true`)
and can reread its identity with `GET /panel/api/nodes/nodeSnapshot/producer/:tokenId`.
Transfer the sender and generation securely to the master. On the master,
`POST /panel/api/nodes/nodeSnapshot/receiver/:id` (`sender`, `generation`,
`reconciled: true`) durably requires verified pulls and blocks legacy merges.
Ensure the node is ready first: this switch is immediate, failed verified
requests have no fallback, and sync may stop. Full inbound sync and standard
HTTPS verification are mandatory. Existing sender/generation streams cannot be
reset or replaced, including after token rotation. The shared receiver table
requires sender identities to be unique even across global and inbound streams;
conflicts must be investigated, never bypassed by changing identity. The sync
job fetches without holding the master's allocation lock and verifies the
sequence and merges accounting atomically under that lock. Selected-inbound
mode, nested nodes, other-admin inbounds, DB backup rollback, cross-host fencing,
and manual reconciliation workflow remain unresolved. The endpoint and the
manual confirmation do not lift reseller quota blocks or prove that unpolled
usage is complete. Sequence state must never be reset to clear an error.
