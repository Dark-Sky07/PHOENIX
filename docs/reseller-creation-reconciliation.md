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

## OpenVPN credential safety

OpenVPN profile issuance now checks the canonical client and unique enabled
traffic identity against local and Go-normalized upstream usage, expiry and
quota while holding the participating allocation lock. A disabled, exhausted,
ambiguous or corrupt identity is rejected; a failed evidence query or pending
journal also blocks issuance. Periodic OpenVPN/L2TP/SSTP/PPTP client filtering
uses the same quota checks even before the next disable job updates its flag.
The direct local runtime payload filter for TUIC and these daemons uses the
same checks in its caller's transaction, including any existing canonical
record; malformed client quota fields are omitted rather than made unlimited.
JSON integer settings retain their exact precision. These are point-in-time
checks and do not capture unpolled traffic, fence nonparticipating writers,
or prove that previously accepted sessions stopped. Settings-only legacy
clients without a canonical record remain supported when their traffic identity
is unique; they need separate historical review.

Legacy certificate filenames sanitize client emails and may collide. A profile
request now refuses a stored certificate with a different CN, mismatched key,
or partial material, and daemon reconciliation checks requested filenames
before stopping the existing daemon. Provisioning, profile reads and pruning are
serialized within one panel process. Existing profiles potentially exposed by a
past collision require investigation; this check does not revoke them. It also
does not coordinate another panel process or a privileged filesystem writer.

**Removing a local client certificate file is not revocation.** A previously
issued profile still contains its certificate and private key, and the current
OpenVPN server configuration accepts certificates signed by the inbound CA;
it does not check the current enabled-client list at authentication. Disabling
or removing a client is therefore not proof that a copied profile cannot
reconnect, nor that an existing session was disconnected. Administrators should
restrict access to affected OpenVPN listeners while investigating and retain
accounting evidence. A migration-safe revocation/allowlist and verified drain
remain necessary before relying on quota enforcement or removal.

## OpenVPN management polling

OpenVPN status version 3 is tab-separated, including empty columns; its client
rows are parsed according to the header rather than whitespace tokenization.
Malformed client rows, incomplete client headers and duplicate normalized
client names reject the whole poll, retain the in-memory baseline and do not
fall back to a later snapshot. This prevents silently treating a damaged response
as an empty or partial client list. Successful observations from other daemons
are journaled and committed before a partial poll delays daemon reconciliation.
A shared in-flight marker fences participating quota operations before polling;
the resulting batch has an identity snapshot, a durable journal and a
payload-bound receipt. A failed write is retried in-process without polling or
restarting the daemon again. On crash, the OpenVPN journal uses a `.manual`
extension, so neither current nor older Xray recovery replays it automatically
without generation evidence. Orphan markers and manual journals need review.

Poll errors themselves are logged but are not durable incidents. A marker or
journal does not prove an interval was complete: the management baseline still
advances before journal publication, the daemon can outlive the panel, and
traffic after the last observation or client disappearance can be missed.
Reconciliation and shutdown are not a verified drain. The accounting lock only
coordinates participating mutation paths; direct runtime changes remain outside
this fence. Do not treat this as complete accounting or automatic recovery.

## Local OpenVPN removal fence

Local OpenVPN client removal, detachment and reseller unassignment are blocked
when the relevant attachment is known. Local inbound removal and protocol
conversion are blocked regardless of attachments until disconnect and final
accounting are verified. A disabled inbound
is not proof of a complete final poll. This is a conservative interim fence:
there is no automated proof or resume workflow yet, the identity scan does not
lock concurrent inserts, and it cannot identify an orphan daemon whose inbound
and attachment evidence were already lost. It does not make OpenVPN traffic
crash-durable or stop active sessions by itself. Preserve evidence and investigate
before any manual recovery; do not infer missing usage.

## Local TUIC shutdown journal

TUIC reconciliation fingerprints use structured, hashed user and listener
settings rather than delimiter concatenation. OpenVPN, L2TP and SSTP/PPTP
fingerprints likewise encode fields and credential boundaries before hashing;
their restart and accounting safeguards still apply. Differently structured client or
ALPN lists cannot alias merely because credentials contain separators. A live
QUIC test checks that a removed user in such a case is disconnected and its
observed counter is retained; this does not prove durable final accounting.

For retained TUIC shutdown batches, a nonzero sample now needs an existing,
unique client-traffic row identified by its recorded row ID. Missing IDs,
deleted rows, ambiguous case variants and negative deltas retain the journal
instead of silently discarding or multiplying those bytes. A point-in-time scan
also checks Go-normalized whitespace and Unicode case variants that SQL
LOWER/TRIM can miss. It is not a predicate lock against concurrent inserts or
renames, and the existing SQL check still rejects destinations it cannot resolve.
Supported row renames
continue to use the row ID. Unexpected or incomplete journal entries also stop
replay, and the local dashboard and participating allocation checks report or
block on any TUIC journal entry or inspection error. Periodic TUIC reconciliation
is deferred while replay fails; listeners are not deliberately stopped. Journal
publication no longer overwrites a conflicting payload, and Linux recovery
rejects symlinks and special files with a 64 MiB read limit. New transactional
TUIC receipts bind the ID to a versioned hash of the original deltas; a legacy
receipt without a hash cannot authorize replay and requires manual review.
Database migration requires the upgraded receipt table and preserves its hashes.

Periodic TUIC polling now creates a shared in-flight marker under the allocation
lock before draining the manager, journals the exact delta batch, and commits a
payload-bound receipt before attempting quota enforcement and clearing the
journal and marker. Periodic daemon reconciliation runs behind a second marker:
it reads the desired state under the lock and immediately collects and journals
any counters drained by stopping or replacing a daemon. An ordinary failed step
retains the batch and marker but releases the allocation lock so independent
recovery can proceed. Before retrying, the collector reacquires the recovery
lock and requires its marker to be exclusive. An uncertain marker deletion
instead keeps the lock until the marker is restored.
After restart, a periodic journal can be cleaned up automatically only under
the recovery lock with its exclusive marker and a matching payload-bound receipt
proving the transaction already committed. Uncommitted journals lack a durable
ownership snapshot and remain for manual reconciliation; they are not applied
automatically. TUIC collection retains negative-only counter evidence for
rejection and keeps distinct source identities separate in the manager's pending
queue. Active TCP and UDP counters saturate at the signed 64-bit maximum
instead of wrapping; excess beyond that limit cannot be represented exactly.
Retired-user removal is serialized with the last session's departure so
a relay write after a counter swap cannot be dropped by that removal. These
measures do not prove immutable historical ownership or generation.
Quota enforcement also disables a client with a negative local or upstream
counter, including an unlimited client, without erasing the corrupt evidence;
review and reconciliation are required before re-enabling it. Automatic renewal
checks both local and upstream counters before resetting any candidate; corrupt
renewal evidence postpones that renewal batch rather than deleting it. Supported
client counter resets, manual rewrites, and client-stat deletion helpers also
reject negative local or upstream evidence, including for non-resellers. Single,
scoped, bulk and inbound-client removal paths also preflight this evidence when
asked to delete traffic, before changing associations or runtimes. This is not
proof that every deletion path or runtime mutation is atomic. Preflight checks
precede existing runtime re-enable paths, but those side effects and the
database reset are not a single atomic operation. Removing a local L2TP, SSTP,
or PPTP inbound, or converting it to a different protocol, is deferred even
when disabled: this implementation has no
verified stop-and-final-accounting proof for the whole inbound, and a missing
session marker alone is insufficient. An operator must reconcile it; no
automatic resume path is provided yet. Inbound edits retain an omitted daemon
client list before normalization and reject unproven PPP client removal from
settings or canonical links. A missing linked client or blank identity in a
scoped PPP inbound also blocks removal pending manual reconciliation, even when
the requested email is different; its ownership cannot be inferred. A removal
request with a blank accounting identity also fails closed rather than silently
skipping the identity check (an empty scope remains a no-op). Local daemon
inbound edits now reject a failed
traffic-filter query instead of applying raw, unfiltered credentials; creation
uses the same rule. TUIC, OpenVPN and PPP daemon filters omit credentials with
missing, ambiguous, or SQL-unresolvable normalized traffic identities rather than stopping the
entire node. Periodic OpenVPN, L2TP, SSTP and PPTP jobs now apply the same
identity policy to canonical linked clients rather than trusting preloaded
traffic rows or raw inbound credentials. A failed database check defers that
reconciliation; an empty successful filter remains explicitly empty. This does
not prove old daemon sessions stopped or that their final traffic was recorded.
The dashboard reports a point-in-time count for enabled local daemon settings
and warns when that diagnostic is unavailable; it does not inventory canonical-
link-only clients, old sessions, disabled inbounds, remote hosts or historical usage.
Runtime apply failures request recovery. Updating an existing client traffic row to
an enabled state also checks the proposed quota and expiry against local and
upstream counters, rejecting exhausted or corrupt values rather than silently
reactivating it. This check does not establish complete historical usage.
Runtime and database updates remain separate.
Node status counts use overflow-safe
local totals but do not replace global enforcement.
Ordinary cleanup failures release the lock behind the retained
marker; an uncertain marker deletion keeps the lock. A crash before journal
publication or after journal removal can
leave only a marker; never infer missing bytes from it.

This is not a complete TUIC accounting protocol. Other hosts cannot inspect this
host's journal. The safeguards do not prove client generation or reseller
ownership, protect unpolled in-memory traffic against a crash, or retroactively
bind old TUIC receipts to their payloads. Normal panel shutdown now creates a
marker before stopping TUIC listeners and commits their collected counters using
the same receipt path. Concurrent TUIC server Close calls now all wait for the
same handler drain before the manager collects counters; this is not evidence
of durability across a crash or of traffic outside the tracked handlers.
If prior recovery or marker creation fails, shutdown
still closes listeners and attempts to save their drained counters as a `.manual`
journal requiring operator reconciliation; it is never automatically replayed.
If publication fails before the file exists, counters are requeued in memory;
a crash before publication or an uncertain filesystem sync can still lose them.
The local journal is not visible to other hosts. Other runtime mutation paths can still drain sessions
outside this barrier. A marker does not prove that connections stopped or all
final bytes were observed. Enforcement
following the reconciliation drain may require another reconciliation tick.
Operators must preserve evidence and reconcile rather than deleting a journal or
inventing usage.

## Xray poll identity capture

For journaled Xray and L2TP deltas, a nonzero sample with a blank identity or
an unchanged identity with no unique client-traffic destination now fails
closed, including for non-reseller clients. The journal and poll marker remain
for evidence-based review instead of silently dropping or multiplying usage.
Before applying, point-in-time scans of client, traffic and reseller identities
also reject Go-normalized matches omitted by SQL LOWER/TRIM. These scans do not
prevent concurrent phantom inserts or renames and require full table reads.
This does not infer ownership or recreate missing rows; the existing verified
reseller-archive path remains available for changed identities. Reusing a local
journal ID now requires the saved and retry payloads to match; publication does
not overwrite another payload. Linux journal reads reject final symlinks and
special files, and journal records are capped at 64 MiB; oversized records stay
pending for review. This relies on a trusted journal directory and filesystem
support for hard links. New transactional receipts bind the ID to a versioned
hash of the serialized payload, including the captured identities; conflicting
replays fail closed. Legacy receipts lack that evidence and require manual review
if replayed or paired with an unresolved marker. A hash is not sender
authentication and does not make an unseen journal on another host visible.

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
versioned Xray receipt records that a traffic transaction committed. Legacy
receipts without a payload hash no longer authorize automatic marker cleanup;
their markers need manual review. Recovery still cannot inspect journal copies
on other hosts. It retains the
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

The agreed rollout is **not complete**: inbound node snapshots remain legacy
unless explicitly provisioned for the experimental verified path; the dashboard and limited reseller guard described below do not cover all write
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
disabled nodes and nodes with either experimental verified direction enabled,
because neither proves that historical or unpolled usage is complete. The warning
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
and to nodes with either verified snapshot direction: historical or unpolled
usage is not proven complete. Normal quota/accounting mutation callers must hold the allocation
lock. The rule can block otherwise healthy resellers until an incoming protocol
and an evidence-based migration exist; it does not shut down the node. It is
not a complete perimeter: direct administrator/runtime/database writes,
unrepresented or lost associations, upstream global-only traffic and other
nonparticipating paths may bypass it. Existing totals can still be incomplete.

The experimental node-to-master snapshot endpoint
`POST /panel/api/inbounds/verifiedNodeSnapshot` requires a full bearer token and
an independently provisioned `node-snapshot:api-token:<id>` sender state. It
captures local database traffic without the global overlay, uses a distinct
node-snapshot key, binds the response to a 32-byte request challenge, and
reserves its sequence while holding the allocation lock.
It rejects missing traffic, ambiguous identities, malformed settings, nested
nodes, and other-admin inbounds rather than signing a partial view. It does not
poll Xray or establish completeness of historical or unpolled usage. The Nodes peer setup dialog and interactive-superadmin API stage a node producer with
`POST /panel/api/nodes/nodeSnapshot/producer` (`tokenId`, `reconciled: true`)
and can reread its identity with `GET /panel/api/nodes/nodeSnapshot/producer/:tokenId`.
Transfer the sender and generation securely to the master. On the master,
`POST /panel/api/nodes/nodeSnapshot/receiver/:id` (`sender`, `generation`,
`reconciled: true`) first probes a fresh nonce-bound signed snapshot using the
configured token, then durably requires verified pulls and blocks legacy merges.
The probe sequence is fenced and never merged as accounting; subsequent pulls
must have a higher sequence. The switch is immediate after the probe, failed
verified requests have no fallback, and sync may still stop if conditions change.
The node must have a clean configuration before and after the probe; full inbound
sync and standard HTTPS verification are mandatory. Existing sender/generation streams cannot be
reset or replaced, including after token rotation. The shared receiver table
requires sender identities to be unique even across global and inbound streams;
conflicts must be investigated, never bypassed by changing identity. The sync
job fetches without holding the master's allocation lock and verifies the
sequence and merges accounting atomically under that lock. Selected-inbound
mode, nested nodes, other-admin inbounds, DB backup rollback, cross-host fencing,
and manual reconciliation workflow remain unresolved. The endpoint and the
manual confirmation do not lift reseller quota blocks or prove that unpolled
usage is complete. Sequence state must never be reset to clear an error.

### Retaining incomplete remote-association evidence

Participating reseller quota and renewal checks also reject missing inbound
references and any surviving node traffic baseline for an owned client, even
when its client-inbound link is missing. Startup orphan cleanup retains links
for clients still mapped to a reseller so it cannot silently erase that
warning. These checks may block a genuinely retired remote association until
manual review. Do not delete a baseline or link merely to unblock quota; use
runtime and accounting evidence to reconcile first. This cannot detect an
association or ownership mapping that was already erased, fence external
writers, or recover bytes never recorded by a runtime.

### Database migration and verified traffic evidence

SQLite-to-PostgreSQL migration and PostgreSQL-to-SQLite export now include
counter incidents, global overlays, Xray batch receipts and in-flight markers,
and verified snapshot sender/receiver sequence state. A source missing any of
these tables or their columns is rejected before destination data is cleared;
upgrade and investigate its history before retrying. Creating missing tables
does not restore any evidence already lost. Earlier exports or migrations that
omitted these tables require manual review: never reset a stream or assume
missing receipts mean no old batches exist.

On PostgreSQL import, clearing and copying destination rows now commit in one
transaction; a copy error rolls the row changes back. Schema changes precede
that transaction. SQLite export now copies into a private temporary file,
closes and syncs it, then replaces the destination; a copy failure leaves an
existing destination intact. Existing destination WAL, shared-memory or journal
sidecars block replacement. Directory-sync errors can be reported after the
replacement has happened; inspect the destination before retrying.
The copy is not an online consistency or runtime fencing protocol. Stop writes
and runtimes, preserve and reconcile on-disk journals/fallbacks separately,
and take consistent backups before migration; an incomplete copy, concurrent
writer, prior data loss, or database rollback cannot be corrected by this change.

### Disappearing Xray counters

If a previously observed Xray user uplink or downlink counter disappears from a
later complete stats response, the poll is rejected without advancing any
baselines. A persistent incident and local fallback retain the prior reading;
`missing-uplink` / `missing-downlink` means no new reading was observed, not
that the observed value was zero. Collection and participating quota changes
remain blocked for manual review even if the counter reappears. Legitimate user
removals can also trigger this conservative barrier. This detects an observed
omission, not an exit between polls, undetected resets that overtake the old
value, incomplete runtime coverage, or usage never reported to the panel.
Client-quota checks now consult poll, journal and incident barriers even for
unlimited resellers, rather than returning early before these checks.

For a missing or decreasing user counter, the collector retains the shared
pre-poll marker even if writing both incident stores fails. The marker has no
batch receipt, blocks participating allocations on other hosts using the same
database, and is not automatically cleared by receipt recovery. Investigate
and reconcile it together with any incident or fallback evidence; do not simply
delete it. This protects only after the pre-poll marker committed and while the
shared database and cooperating checks remain trustworthy. It does not capture
unpolled bytes or fence operations already in progress.

Node snapshot processing now refuses to sweep a reseller's surviving node
baseline when its client link has disappeared. A second check prevents cleanup
from deleting an owned client and its baselines across all nodes when the final
link disappears during synchronization. These checks preserve evidence for
review, not proof that the remote runtime is inactive; already-erased ownership
or baseline records remain outside their coverage.

### Quiescence capability investigation

The checked Xray core version (`64fada32b5b9`) does not establish the proposed
per-user drain through its existing `RemoveUser` RPC. VLESS removes the user
from its validator and VMess removes it from user maps; neither API path proves
that already-established connections have stopped. For example:

- https://github.com/XTLS/Xray-core/blob/64fada32b5b9/proxy/vless/inbound/inbound.go#L244-L247
- https://github.com/XTLS/Xray-core/blob/64fada32b5b9/proxy/vmess/inbound/inbound.go#L177-L185

The online map excludes localhost addresses, and online statistics can be
disabled or unsupported; an empty list is not proof that all connections have
ended:
https://github.com/XTLS/Xray-core/blob/64fada32b5b9/app/stats/online_map.go#L35-L39

Do not use RemoveUser, a fixed delay, an empty online list, a final poll, or an
operator checkbox as a quiescence certificate. No automatic quota unblocking
has been enabled. A safe implementation needs a connection-fencing/drain
capability that accounts for all relevant protocols and sessions, prevents new
connections and nonparticipating writers throughout final capture and commit,
and fails closed across crashes and remote nodes; its guarantees must be tested
against real cores. Externally running cores and already missing usage also need
independent evidence. The affected-node/no-node-shutdown policy remains intact.

The CI characterization test starts the pinned Xray binary, opens a real
VLESS TCP stream through it to a local echo server, removes the user over the
API, and tests whether that existing stream still carries data. It characterizes
one pinned core and protocol, not all release binaries or protocols. In
particular, it is a regression warning against assuming RemoveUser drains
sessions; it is not a quiescence implementation.

### Experimental VLESS connection termination

An **experimental, test-only** patch for the pinned Xray source is available at
`patches/xray/experimental-vless-disconnect.patch`. It tracks authenticated
VLESS transport connections per user object, removes authentication and closes
tracked connections when that user is removed, and rejects a request that
finished authenticating against an already removed/replaced user. It serializes
registration and removal on the inbound handler. The build script applies it
only to the exact pinned module version in a temporary copy; it does not replace
release binaries or configure nodes. CI exercises an established TCP connection
and races registration against removal.

This is not a production disconnect or reconciliation guarantee: other
protocols, multiplexed and encrypted transports, UDP, reverse sessions, process
crashes, remote nodes, handler replacement and nonparticipating writers need
separate investigation. `Close` does not prove final counters were polled or
committed, and removal cannot retract traffic already accepted in a race. Do
not use this patch to lift quota blocks, delete reseller accounting evidence, or
claim safe final consumption; release and upgrade flows still use upstream Xray.

An analogous experimental Trojan patch is also built in CI. Its TCP test checks
that deleting one authenticated user closes that user's stream while another
user's stream remains usable, and a race test covers stale authentication and
registration. Trojan UDP-over-TCP is not independently tested. As with the
VLESS prototype, neither patch supplies a final counter flush or a durable fence,
and neither is enabled in release or quota reconciliation.

The experimental build also includes VMess and legacy AEAD Shadowsocks TCP
patches. CI exercises each with a real Xray SOCKS client and two user streams,
as well as handler registration/removal race tests. The legacy Shadowsocks UDP
handler caches an authenticated `inbound.User` and uses it for subsequent
packets; removing a user from the validator alone does not revoke that cached
UDP flow. The experimental patch also tracks those UDP associations. Its bounded
real-packet test checks an established and a fresh association for the removed
user and verifies another user's traffic continues. It does not establish
quiescence of in-flight packets or counter writers. Shadowsocks 2022 has a
separate experimental multi-user patch, described below.

For every experimental protocol, closing a transport is not equivalent to
waiting for forwarding goroutines and counter writers to finish: late buffered
traffic and asynchronous writes must be drained and then durably merged before
quota is released. A process crash during that process and node rollback also
need fail-closed recovery. Until an authenticated, versioned capability with
these properties is implemented and deployed, the current upstream release
and reconciliation guards remain unchanged.

The VMess and Shadowsocks SOCKS reconnection tests observe no forwarded data for
five seconds after removal. They do not prove immediate closure of an invalid
new connection: Xray's anti-probing behavior can intentionally delay rejection.

Shadowsocks 2022 multi-user uses sing's authentication service. In the pinned
source the service authenticates an index and the callback indexes a mutable
`users` slice; removal changes that slice. The experimental patch uses stable
user object identities, synchronizes the dependency's authentication-map
snapshots, rejects stale callbacks and tracks stream and packet sessions. It
applies only to the multi-user inbound, not the single-user or relay variants.
Real TCP and UDP tests and race tests are limited evidence, not a quiescence or
accounting certificate. This remains test-only and cannot justify quota release.

The experimental VLESS removal now waits (subject to the RPC context deadline)
for its tracked inbound handler scopes to exit. A timeout returns an error and
does not restore the removed credential. This is **not** a final accounting
barrier: the pinned core's `task.Run` may return after one task errors while a
sibling is still executing, and asynchronous dispatcher work may also outlive an
inbound handler. `TestXrayTaskRunMayReturnBeforeSiblingCompletes` characterizes
that behavior. Do not treat handler exit or a stable-looking counter as proof
that no subsequent counter writes are possible. Other protocols do not have this
experimental handler wait.

The VLESS prototype also rejects re-adding the same normalized email or VLESS
credential while removal waits for handler cleanup. If removal times out or a
close fails, its in-memory fence remains in place for that handler. A successful
wait clears the fence and does not prevent a later re-add; restart, other
inbounds, other nodes, direct writers and untracked asynchronous traffic are
outside this mechanism. This is neither a durable reconciliation fence nor an
operator recovery mechanism and cannot authorize final quota release.

The pinned dispatcher's `DispatchLink` has a separate late-accounting hazard:
it wraps the uplink in `buf.TimeoutWrapperReader`. When a timed read returns
without data, its underlying read goroutine can subsequently finish and leave
bytes buffered inside the wrapper; the counter is incremented only when a
later read consumes those bytes. `TestXrayTimedOutReadCanAccountBytesLater`
reproduces this ordering deterministically against the pinned code with the race
detector. Consequently even a completed underlying read and unchanged counter
are not enough to establish final consumption. Any future drain design must
include pending wrapper reads and account for their buffered bytes, without
counting them twice or dropping them. This test does not cover every dispatcher,
transport, or writer, and does not authorize quota release.

The test-only Xray build now applies
`patches/xray/experimental-timeout-accounting.patch`: an asynchronous timed
read accounts its returned bytes when the underlying read completes, even if
no caller later consumes the retained buffer. Both timed and ordinary later
reads avoid adding those bytes again. Race tests exercise abandoned reads,
errors carrying bytes and later consumption. This changes the timing of
accounting for bytes already read and needs further protocol and production
review. It neither waits for a pending read to finish nor proves that a final
poll or commit is complete, so it remains experimental and does not affect
release binaries or lift any quota guard.

Administrative reseller enable/disable is not an atomic runtime operation.
The account state and login epoch are saved before enforcement across inbounds
and isolated clients. Partial enforcement failures are now returned to the
caller, and local restart requests are propagated even on failure. A repeated
**disable** retries eligible resources without bumping the login epoch again;
a redundant enable remains a no-op to avoid unexpectedly re-enabling clients
that an administrator disabled independently. A failed enable may therefore
need manual investigation and resource reconciliation. Remote dirty markers
and local restart requests are recovery signals, not evidence of closed
sessions, complete traffic accounting, or successful final synchronization.

Client edits now apply local PPP VPN and AmneziaWG updates only after the
settings transaction commits. Bulk detach and bulk delete also trigger the
local TUIC/AmneziaWG reconciliation after settings synchronization. Tests
exercise a running TUIC listener and AmneziaWG peer index, including failed
bulk saves. These checks establish that the intended local runtime update is
attempted with committed settings, not that every live tunnel/relay and its
accounting has drained. TUIC and AmneziaWG immediate-apply helpers now reject
filter-construction failures rather than applying raw settings; they log and
return errors, and client mutation paths request recovery on failure. Most
mutation callers still return success with a restart request after an apply
failure, not a durable per-client incident; an Xray restart does not itself
prove tunnel recovery. Periodic reconciliation remains a backstop, and PPP
shutdown and residual child/session lifetime need further verification. These
paths must not be treated as quota-release barriers.

The client-add transaction now applies local PPP and AmneziaWG changes after
commit, and marks a remote node dirty if its add was attempted but the local
transaction fails or has an uncertain commit. A remote inbound update no longer
implicitly creates a new inbound when resolving the original tag fails: an
absent tag or an inconclusive node response requires separate reconciliation.
This deliberately favors safety over automatic upsert and can leave a node
pending until the missing configuration is investigated. Dirty state does not
prove the remote add was undone or the accounting history reconciled.

Synchronous local L2TP/SSTP/PPTP client reconciliation now constructs its
credentials from the traffic-filtered runtime settings instead of raw inbound
JSON. If the filter cannot be read, it returns an error without applying raw
credentials. Enabling local OpenVPN/L2TP/SSTP/PPTP through `SetInboundEnable`
also prepares the filtered payload before persisting the enabled state when a
runtime is available; a filter failure leaves the inbound disabled. These
changes prevent a failed filter from reinstalling quota-disabled credentials,
but a failed apply can leave the prior runtime running. They do not prove
session termination, final PPP traffic accounting, or complete recovery.

SSTP/PPTP replacement now waits for the supervised daemon's exit signal; if
that signal is missing or times out, the old manager entry is retained and a
replacement is not started. Checked removal returns the failure rather than
pretending the parent exited. This is **not** a barrier for pppd children,
active sessions, hook spool files, or counters; the unchecked periodic removal
path logs failures and remains a recovery backstop.

L2TP sampled deltas now use the existing identity-bound Xray journal format,
transactional receipt and durable in-flight marker under the participating
allocation lock. Cron and dashboard job instances serialize the sample and its
retry, retain a failed sample, and sample before their own periodic daemon
reconciliation. Tests cover failed writes, an uncertain committed retry and
journal replay after simulated process restart. A failed interface-counter read
also defers the periodic reconcile without advancing the baseline. Unreadable or
malformed L2TP samples now retain the known baselines as evidence and publish a
shared incident before the poll marker is cleared; an incident-write failure
retains the marker. Even a transient read failure can therefore require manual
review. Collector marker reads are root-confined and bounded, rejecting special
files and symlinks on Linux, and unsigned interface counters above the signed
accounting range are rejected rather than silently saturated.

This only protects observed deltas. L2TP interface baselines are still held in
memory; a process change, missed ip-down counter, crash before journal creation,
concurrent runtime mutation or usage after the sample can leave consumption
unknown. A marker without a receipt or journal requires manual review. Journal
replay on another host is not guaranteed, and the Xray-named marker/journal is
shared with these L2TP samples. A real session/counter drain and durable
session baselines remain necessary before reconciliation or quota release. Do
not infer missing bytes or treat a stopped parent daemon as proof of completeness.

Newly generated L2TP ip-down hooks now create and sync a persistent incident
under `l2tp/hook-incidents` and leave the active marker in place. They retain
the hook identity, the prior marker if readable, and available interface counters
as investigation evidence; a late down hook no longer deletes a newer marker.
Updated ip-up hooks refuse to overwrite an existing marker and leave an incident
on collision or other reportable failure. Markers and incidents require manual
review, not blind cleanup. Even a session that starts and ends between polls can
therefore leave an incident when the updated hook runs. The collector,
sensitive removals and local incident publisher observe the incident; the
publisher also creates the shared quota barrier. Ordinary disconnects now
require manual reconciliation. Visible hook incidents defer sampling; a separate
persisted accounting warning can allow another sample but now always defers
periodic daemon reconciliation when publication reports an error. A shared
L2TP incident also defers that reconciliation on participating panels even if
their local incident file is absent; an unavailable shared incident store is
not treated as clear. This can delay L2TP configuration changes on other hosts,
but does not automatically stop their running daemons. Publication
is checked again after a successful poll, and after an uncertain poll-marker
restoration or retained batch releases its lock, avoiding recursive lock
acquisition; hooks can still race the final check. The
collected counters are not certified final consumption and are not
automatically credited. Existing running daemons may still use an older hook
until their configuration is regenerated; a missing/failed hook, interruption
before its marker is durable, cross-process races, failed incident publication,
nonparticipating writers and lack of a final drain remain outside this measure.
The node is not automatically stopped by the incident.

Updated L2TP ip-up hooks also create and sync a `pending.*` file before changing
session markers, and remove it only after syncing the marker. The hook stages
and syncs the complete identity in a second pending file, then publishes it with
a no-replace hard link; readers cannot see a partially written new marker from
this hook. This requires `ln -T` and hard links between the incident and session
directories; if unavailable or on different filesystems the hook fails closed
and leaves evidence for review. Existing hooks are not upgraded until daemon
configuration is regenerated. While observed,
the collector retains its shared poll marker and retries under the allocation
lock only if no other unresolved marker exists. The collector checks hook
evidence even when it has no tracked running daemon, rather than treating a
visible pending hook as an empty sample. Sensitive operations defer;
a pending file that persists after a killed hook needs manual investigation,
not automatic deletion. If a pending entry disappears while being inspected,
the current scan still defers instead of declaring a permanent read incident;
other read errors and nonregular entries continue to fail closed. The collector
and removal scan recheck hook evidence after an empty or completed scan, but do
not synchronize with hooks and cannot close the observation race. A failed,
uncertain marker deletion is restored while
the allocation lock is held; if restoration fails, the collector retains that
lock and retries. A process or database session failure can still release the
lock before restoration, so this is not a durable fencing guarantee in that
case. The hook does not take the database lock: it can begin or complete between
checks, including before pending creation or after the last observation. A crash
before pending is durable, old hooks, and other hosts before observation remain
unprotected. Pending evidence does not prove a drained session or final usage.

When an L2TP session that was sampled in this process disappears from the next
snapshot, polling now preserves all baselines and records the missing sessions
and the remaining snapshot as evidence. A surviving session marker whose
interface has disappeared is also treated as an incident even if no prior sample
exists; evidence explicitly says the prior count is unknown, not zero. If the
tracked runtime becomes unavailable while sampled
sessions exist, all last baselines are likewise recorded as uncertain. The
collector also inspects surviving session markers, including those without a
baseline alongside previously sampled sessions: each unobserved session is
reported with an unknown prior count. Unreadable markers or counters fail closed
rather than being treated as an empty sample. If a hook is pending while known
baselines exist, the lost runtime remains a durable incident rather than being
mistaken for a merely temporary hook wait.
Parent exit is not treated as proof of child exit or complete usage. A shared incident
fences sensitive quota operations after the evidence is
durable; a failed incident write leaves the poll marker in place. This is
intentionally restrictive: ordinary disconnects of sampled L2TP sessions can
require manual reconciliation because the existing down hook does not save final
usage. The node remains running, but automatic daemon reconciliation is deferred
while local incident publication reports an error; sampling can also be deferred
when the underlying hook or runtime evidence remains uncertain. Other active
L2TP sessions can therefore accumulate unsampled traffic during the hold. It cannot
detect a session that began and ended between polls when no marker remains,
disappearance after a process restart or runtime replacement, or interface reuse with the same name
and higher counters. It does not recover unobserved bytes.

L2TP daemon replacement and checked removal now preserve their tracked process,
interface baselines and runtime directory if either tracked strongSwan/xl2tpd
parent exit cannot be observed. Dashboard stop/restart and local runtime removal
report that error; periodic reconciliation retains state and reports it in the
runtime status. Parent exit is not evidence that pppd children, hooks,
active sessions or outstanding traffic have drained, so it does not authorize
quota release or claim that removal disconnected every active user.

L2TP session scans now reject unreadable, empty or partially written username
markers and unexpected interface lookup errors instead of interpreting them as
an empty session. The current in-memory baseline is left unchanged and the
periodic reconcile is deferred until a successful read. An interface that is
already gone, a marker removed before the directory scan, or a daemon that has
stopped can still hide unobserved consumption; this check does not solve those
loss windows or itself create a durable accounting incident.

After panel restart, numeric L2TP runtime entries not owned by the current
manager now block L2TP reconciliation, checked removal, dashboard stop and
restart; the runtime status reports that manual review is required. They are
not signalled or deleted. The former PID-name-based orphan killer, which also
used the host-global strongSwan PID directory and could target an unrelated
service, has been removed. Configuration generation also no longer deletes
other `*-l2tp` strongSwan directories based only on their names; they may belong
to another service or hold unreconciled evidence and require manual review.
A tracked inbound must be removed before replacing it with a different inbound. Other panel services remain running; an existing
untracked L2TP daemon may also continue running with its prior configuration.

This is a conservative operational barrier, not complete recovery: it does
not prove process ownership, stop untracked sessions, or reconstruct lost
usage. Missing runtime entries do not prove no orphan exists, and the directory
check is not itself a cross-process fence. Operators must preserve the evidence and independently reconcile traffic
and process/session state before resuming; merely deleting the directory is not
proof of safety. A controlled evidence-backed recovery workflow is still needed.

A locally observed untracked L2TP runtime or an uninspectable incident store
now blocks participating reseller quota/renewal checks and checked client or
inbound removal, and appears in the dashboard's accounting warning. The job
and quota checks retain a separate local `l2tp-accounting-incident` file in the
configured DB folder, so disappearance of the runtime directory alone does not
clear the warning. If writing it fails, the process retains an in-memory retry
barrier. Xray's poll lock is deliberately not blocked by this L2TP-specific
marker, so unrelated traffic collection can continue.

The L2TP collector also checks for untracked runtime directories before sampling,
including when it owns no daemon. It retains known baselines and records a read
failure as evidence rather than interpreting the orphan as an empty sample.
If incident persistence fails, the existing shared poll marker remains for
manual recovery. This protects only observed directories; a runtime that appears
after the scan or leaves no directory remains outside this check.

The L2TP job also retries publication of a shared database incident (reserved
node ID -2) under the participating allocation recovery lock. After the insert
commits, other upgraded panels sharing that database reject participating quota,
renewal and destructive checks, while ordinary Xray traffic collection can
continue. The database evidence migrates with existing counter incidents. A
single shared incident does not identify every affected host and is never cleared
automatically; a remaining local file republishes it on the next job tick.

This is not a complete cross-host fence: another host can act before observation
or while publication is waiting or has failed, and a scan can miss an orphan
without a runtime directory. If both the local file and database writes fail,
the in-memory retry is lost on process exit. Unguarded writers, lost advisory
sessions and unsampled sessions remain outside the protection. An operator must
preserve and reconcile evidence across all affected hosts and confirm process,
session and historical-traffic state before clearing either local or shared
incident; deleting markers is not proof of completeness or an automatic recovery
procedure.

A decrease in a previously sampled L2TP interface counter is now treated as an
uncertain interval rather than a zero baseline or a new session. The entire
snapshot is validated before any in-memory baseline advances. Under the shared
allocation lock, the poll records the observed interface, client, prior and
current counters in a separate local `l2tp-accounting-evidence` directory, and
records the shared L2TP incident before clearing its poll marker. Existing
orphan-warning files do not overwrite the separate counter evidence. If either
write fails or is ambiguous, the in-flight poll marker remains and participating
mutations and polling may be blocked until manual reconciliation. No missing
bytes are inferred. Repeated decreases stop this L2TP collector from sampling
other sessions until reviewed; do not remove the evidence directory as a
substitute for review. Local evidence is not copied by database-only migration.

This detects observed decreases only. Interfaces and whole sessions can still
disappear between polls, or be reused with counters above the previous reading;
short-lived sessions and usage after the final read are not captured by this
change. Durable closed-session accounting, writer quiescence and recovery remain
necessary.

SSTP/PPTP PPP session checkpoints are now retained after a closed spool file is
acknowledged. This prevents a second collector that already captured the same
record from billing it again after the first acknowledgement. All job instances
also share one process-local run lock. PostgreSQL writers create/lock the same
session checkpoint row before calculating its delta, including on concurrent
first insert. A lower absolute reading is rejected rather than assumed to be a
reset, and the transaction does not advance its checkpoint. Checkpoints are
retained indefinitely, so storage grows; migration requires and copies their
table rather than silently starting empty. Before advancing any PPP checkpoint,
the transaction now requires exactly one normalized matching client-traffic row
for every PPP sample. A missing or ambiguous destination rolls back the entire
batch and keeps the collector's poll marker and spool evidence for review. The
row check does not establish the original client's generation; do not recreate
or merge rows without reconciling their history. Duplicate session IDs within a
PPP scan or accounting batch are rejected, even when identities and counters
appear compatible: they may represent distinct sessions or ambiguous evidence,
so no records are acknowledged until reviewed. The traffic writer converts
private copies of absolute PPP samples, leaving caller-owned batches unchanged
on failure or success so reusing a batch does not turn retries into raw deltas.

After each successful SSTP/PPTP poll, periodic daemon reconciliation now checks
both local PPP evidence and the shared PPP incident before proceeding. Existing
incidents, an uninspectable local store or an unavailable shared incident table
therefore defer reconciliation even if the current poll is empty. Other hosts
sharing the database can also defer their PPP configuration changes, but running
daemons are not stopped by this check. Polling can continue, and a committed
client-disable result still invalidates the dashboard. This check is not a
lock on PPP hooks or a proof that sessions have drained; a new incident can
appear after the check.

These changes only protect the same session ID and participating checkpoint
writers. Different sessions or other traffic writers updating the same client across
processes still need independent serialization. The session ID alone does not
prove immutable client generation; final hooks, children, undetected short-lived
sessions and post-sample traffic are not proven drained. Older deployments may
already have deleted checkpoints, and missing evidence cannot be reconstructed.

A detected lower or out-of-order SSTP/PPTP PPP counter now retains the observed
session identity and prior/current readings as local `ppp-accounting-evidence`,
and attempts a separate database incident (reserved node ID -3) after the traffic
transaction rolls back. Once committed, participating quota, renewal and
client/inbound removal checks on upgraded panels sharing that database are
blocked; the dashboard warns, while unrelated traffic polls are not deliberately
stopped. Failed writes stay pending in process and are retried with the batch;
local evidence remains independent of database migration and must be preserved.

This detects a lower observation, not necessarily a real reset; no missing bytes
are invented. There is an unprotected interval between the rejected transaction
and shared incident publication, and another host can proceed if database
publication fails. If both storage paths fail, a process crash can lose the
in-memory warning. The current batch remains pending and can block later PPP
samples, and parent/child shutdown, final hooks, distinct sessions and writers
are still not fully fenced. Review must preserve evidence and reconcile every
affected host before clearing local and shared incidents.

PPP collection now acquires the participating allocation lock and writes an
in-flight database marker before scanning the spool. It retains the marker
on an uncertain scan or write and retries the same pending batch; the lock is
released between retries so independent journal recovery can proceed. On
reacquiring that lock, a PPP retry now requires its own marker to be the only
unresolved poll marker; another marker must be recovered independently first.
The PPP marker is retained while it waits. An uncertain marker deletion retains
the lock until the marker is restored;
absolute session checkpoints make an uncertain committed write idempotent.
The marker is removed only after a successful write or after a counter-decrease
incident is confirmed in both local evidence and the shared database. Closed
spool acknowledgement follows successful accounting. A crash can leave an
orphan marker requiring manual investigation; it is not proof of a final
sample and must not be blindly cleared. Temporary hook files, unreadable or
malformed records, and unavailable active interfaces now fail the scan instead
of being silently skipped. A spool record must be bounded valid UTF-8, contain
one newline-terminated record and no disallowed control characters; an
unterminated closed record is retained as uncertain evidence rather than
credited and acknowledged. Live spool groups must name a canonical PPTP/SSTP
inbound, and their record's inbound, interface and session identity must match
the group and filename before sampling or scoped removal checks. Legacy closed
records that do not meet these rules need manual review. New staged claims sync
their original spool path before moving a record, and retries validate that path
against the record. Old claims without this provenance require manual review;
scoped removals still defer while any claim exists. The metadata depends on a
trusted local claim directory and is not proof against a privileged writer. This
may block allocation and periodic PPP reconciliation until the issue is corrected. Polling precedes periodic daemon
reconciliation, but other runtime mutation paths, failed/missing hooks, vanished
sessions, traffic after the sample, and nonparticipating writers are not
fenced. Neither this marker nor a successful poll proves session drain or
complete accounting.
When a traffic batch runs under the recovery lock, automatic renewal is deferred
while any other in-flight poll marker remains; accounting the batch itself can
continue. This avoids releasing quota against another poll's uncommitted usage.
A journal without a corresponding marker, external writers, or a lost advisory
lock connection still require separate safeguards and are not covered by this
check. New Xray polls still wait behind an unresolved marker; the recovery lock
only permits replay and other explicitly recovery-aware work.

PPP acknowledgement now claims each sampled closed spool file by an atomic
rename into `sessions-staged` before comparing it with the committed sample.
Hooks cannot replace that claimed path; a changed or malformed record is kept
for retry or manual review, and a crash leaves a discoverable claim. The PPP
poll marker remains until acknowledgement succeeds. An empty claim directory
can represent a live acknowledgement or several interrupted phases. Collection
and sensitive decisions fail closed while it is observed; a claim that persists
requires an operator to reconcile the source record, database checkpoint and
poll marker. A concurrent observer does not create a permanent incident merely
because a successful acknowledgement temporarily leaves an empty claim. Do not
remove any claim merely because it is empty; its existence alone does not prove
that the record was safely accounted for. These measures do not fence a
new hook write after the claim, fix a missing hook, or establish final traffic
or session drain.

Untracked SSTP/PPTP runtime directories are no longer cleaned up by matching
process names and command-line substrings: those do not prove process identity.
The PPP manager refuses automatic start, replacement, or checked removal while
unowned runtime files are present and preserves tracked startup failures. A
local observation records evidence and blocks sensitive quota/removal paths;
the periodic PPP collector also retains its durable shared poll marker for
manual review, even if the directory later vanishes. Review the affected host,
its active children, spool and database before removing evidence or a marker;
a mere directory removal is not reconciliation. Other hosts remain exposed
before observation or if evidence/DB writes both fail; absent directories do
not prove an orphan or unpolled usage is absent. This is not a child-process
termination or final-accounting guarantee, nor does it resolve interface reuse
or the ordering of old and new ip-up/ip-down hooks.

SSTP/PPTP hook records now bind an active session to the pppd parent PID,
process start time, boot ID, and Linux interface index. Hooks require `flock`
and serialize updates per interface; an ip-up cannot replace an existing
active record, and an ip-down with a different owner, peer, or interface index
cannot delete it. Hook failures after creation of the session directory leave a
visible incident; its observation retains a local warning and the collector's
shared poll marker for manual review. Active legacy seven-field records lack
this identity and now block polling rather than being silently adopted; closed
legacy records remain readable. Check and reconcile those sessions during
upgrade. Directory/index checks before and after sampling reduce interface reuse
risk but do not prove that bytes between polls were captured, that hooks always
ran, that interface indices can never be reused, or that a pppd child drained.
Failures before the hook can create evidence, uncooperative old hooks, and the
interval before another host observes a marker remain outside this protection.

Each updated PPP hook now creates and syncs a `hook-pending.*` record before
it mutates a session record, syncs the resulting file and directory, then
removes and syncs the pending record. A killed hook can therefore leave an
inspectable pending record instead of appearing to have completed. While it is
visible, local sensitive decisions and the PPP collector wait; ordinary live
hooks do not create permanent incidents just because a poll overlaps them.
The collector checks again after acknowledgement before clearing its database
marker. An interrupted pending record requires operator investigation, not
blind removal. The mechanism still cannot protect the interval before a hook
creates/syncs its marker, detect a hook that never started, fence hooks that
start after the final check, or prove shutdown and final accounting. Hook sync
requires a filesystem and `sync` implementation that support these operations;
failures leave the operation fenced when their evidence can be written. Invalid
interface names are checked only after pending evidence and the failure trap
are installed; failure to create or sync the initial spool still cannot be
reported reliably by the hook. Unexpected hidden files in session directories
are inspected rather than silently skipped (the hook lock directory is excluded).

Until per-client SSTP/PPTP/L2TP drain and final accounting protocols exist,
known local attachments for these protocols block client deletion and detach,
including disabled inbounds. Matching active, closed, or staged SSTP/PPTP
spool records also block
removal even if database associations have drifted. Admin bulk deletion
preflights its whole batch; lower-level detach and reseller archive/unassignment
paths recheck the guard. This is an intentionally restrictive interim fence: it
does **not** disconnect the user, and there is not yet an automated verified
reconciliation/override flow to resume these removals. Do not remove records or
associations merely to bypass it. An absent spool or association is not proof
that no session exists; nonparticipating writers, concurrent hook/attachment
changes and remote nodes remain outside this local check. The removal guard now
also scans local L2TP session markers, including stale-interface markers after
database and settings drift, and rejects malformed or uninspectable evidence.
These markers do not contain final counters or a stable session generation;
hooks can race the scan or fail to create a marker, so an empty scan is not
proof of disconnection. Panel-side reads are confined to the trusted L2TP root;
Linux rejects final symlinks and special files. The shell hooks, bind mounts,
privileged writers and non-Linux races remain outside this protection. Other
protocols and removal routes still need their own drain and accounting guarantees.

PPP spool directory creation, scans, record reads, claim renames, deletion and
directory sync now use a root-confined filesystem handle. Paths or symlinks
that escape the configured PPP root are rejected rather than accessed during
panel-side collection or acknowledgement. On Linux, record reads reject final
symlinks and non-regular files, and directory opens require directories; FIFO
substitution therefore does not indefinitely block the collector. The runtime
root must still be trusted: these checks do not authenticate hard links or
protect against privileged filesystem replacement. The non-Linux fallback does
not prevent races with concurrent replacement. This does not constrain shell hooks
against a privileged external actor replacing directories after startup, bind
mounts, or other nonparticipating filesystem writers; the runtime root remains
a trusted, privately controlled directory.

### OpenVPN incomplete management responses

A management reply with a legacy title or version but no client header or
client rows is treated as unsafe, not as proof of an empty client list. Such
polls preserve the previous in-memory baselines and stop reconciliation for the
current poll, while observed deltas from other daemons are still committed.
This does not prove the completeness of a syntactically valid response or
recover usage between polls, and a transient poll error is not a durable
per-client accounting incident. Headerless legacy rows use fixed counter
positions rather than searching for a later numeric pair, and counters outside
the signed 64-bit accounting range are rejected instead of silently clamped.
Such malformed polls likewise retain their baselines and require investigation.

### OpenVPN CA and certificate evidence

OpenVPN provisioning refuses to silently replace incomplete, mismatched, invalid
or nearly expired CA material, or to invent a new CA when server or client
material remains. Existing server keypairs must match and be signed by the CA;
existing client certificates must also be signed by it and currently valid.
Partial server material is retained for manual review. Reconciliation validates
material before its no-op path or stopping an existing managed daemon, and
reports errors rather than silently ignoring them. A successful later inbound
in the same reconciliation pass does not clear an earlier inbound error from
the dashboard; the error is cleared on a subsequent successful pass. This is deliberately
fail-closed and may require operator recovery or a planned CA migration; it is
not automatic rotation or revocation. The locks are process-local, and these
checks do not prove daemon termination, final accounting, or protect against
external writers. A running daemon may continue using loaded material.

### OpenVPN stale process evidence

A retained OpenVPN PID file blocks starting a replacement and is preserved for
manual investigation, rather than signalling a PID based on a command-line
substring. A confirmed stop of a process started by this manager removes only
a matching regular PID file; unexpected or mismatched evidence is retained.
Deletion also refuses to remove an inbound data directory containing a PID
file. Recovery after a crash may therefore require operator intervention.
These checks are process-local and do not fence other panels or privileged
filesystem writers. A missing PID file does not prove there is no orphan;
verified process-tree termination, final traffic accounting and a safe
reconciliation override remain unimplemented. Manager stop paths now retain
managed state and firewall rules and report a stop failure, including repeated
attempts after an owned PID cleanup fails. Dashboard stop/restart and local
inbound disable/removal propagate these errors; an unmanaged retained PID also
blocks disable or removal rather than reporting success. Start and restart
responses acknowledge a request, not a confirmed running service. These
checks do not find or fence an orphan without a PID file or prove a daemon or
its children have drained. Do not remove evidence merely to bypass a block.

### OpenVPN legacy subnet collisions

The historic OpenVPN pool maps positive inbound IDs to a /24 but clamps one
address octet, so IDs at or above 64000 can alias earlier pools. New manager,
network, config and profile operations reject IDs outside 1–63999; they do not
silently renumber an existing pool. An invalid enabled inbound aborts the
whole desired-set reconciliation rather than being treated as absent and
stopping healthy daemons. Existing running colliding configurations are not
automatically terminated or migrated; operators must investigate and plan a
migration. Unique IDs within this legacy range do not establish that the
resulting private subnets are unused by other software or networks.

### OpenVPN configuration operands

Listener and profile hosts must be IP literals or single ASCII DNS-like
operands; pushed DNS values must be IP literals. Invalid ports, host operands,
and control characters in the daemon data directory are rejected before
replacing an existing daemon or writing a profile. Generated config paths are
quoted and escaped. This is a stricter compatibility boundary: operators using
unusual hostname spellings, scoped IPv6 literals or malformed legacy settings
must review them. Unit tests exercise generated text and failure paths. CI also
installs Ubuntu's OpenVPN package and checks server and client parsing, including
decoding quoted paths, but this is not a client/server session, a test across
all supported versions, or a session-drain test.

### OpenVPN profile reuse: real-session characterization

CI installs Ubuntu's OpenVPN package and runs a server and client in separate
isolated Linux network namespaces connected by a veth. The client first obtains
a valid profile; the panel's local certificate files are then pruned and a
server is started without that client in its desired list. The previously
issued profile still completes an OpenVPN connection. This demonstrates the
lack of revocation for that binary and scenario; it does not prove behavior for
all OpenVPN versions or deployments, quantify traffic after removal, or test
termination of an already-active session. Do not interpret removal of local
files, quota checks or this test as a revocation or final-accounting barrier.

### Experimental OpenVPN fingerprint admission (test only)

An isolated real-server CI experiment adds a test-only `tls-verify` shell
script and SHA-256 certificate fingerprint allowlist. On the Ubuntu OpenVPN
package tested, an allowlisted Bob completes a connection, Alice's different
certificate is rejected during TLS verification, and a new Bob authentication
is rejected when the allowlist file is missing. The same real-server test
also sends ICMP over Bob's already-established tunnel before and after removing
the allowlist, including after a new Bob authentication is rejected; in this
bounded test the existing tunnel continues forwarding. Admission checks alone
therefore do not terminate established sessions. This is an experiment only:
PHOENIX does not install, generate or enable that verifier in production.
The test uses a trusted temporary filesystem and one OpenVPN distribution; it
does not establish safe shell/path handling for deployment, authenticated or
atomic allowlist updates, multi-process coordination, denial of already-active
sessions, accounting drain, crash recovery or any quota-release guarantee.
The existing production behavior still accepts previously issued profiles.

The same test-only experiment obtains Bob's numeric client ID from a complete
`status 3` response and sends `client-kill`. One CI run showed that a success
response can precede disappearance from status, so the test waits for a later
complete status without Bob. In the bounded follow-up observation, a ping to
the VPN server address reports one packet transmitted and no reply, while the
missing allowlist rejects fresh authentication. With DCO disabled in the
server and clients, tcpdump also observes the ICMP request on Bob's client
`tun0` interface after the session disappears. An earlier run with DCO enabled
reported packet loss and a route via `tun0`, but tcpdump did not see the probe;
its path is not established, and the cause was not determined. A second user
in a separate network namespace remains in status and continues to receive ICMP
replies after Bob's kill. This covers one OpenVPN package, two UDP sessions and
one ICMP path; it does not prove all traffic or child processes are drained,
prove final counters were captured, or fence races between status, kill,
reconnect and allowlist changes.
This management sequence is not wired into production or quota release.

The live experiment also records the `bytes_received` and `bytes_sent` variables
from a test-only `client-disconnect` hook. After the last management status
sample, Bob sends and receives more ICMP traffic; in the tested run the hook
reports both counters greater than that sample after the management kill.
This suggests a possible source of additional accounting evidence, not a
production accounting implementation: the test file is not journaled, synced,
authenticated or replayed, and hook execution is not guaranteed on process or
host crash. The test does not prove that packets still in flight are included,
that the event belongs to a durably identified client generation, or that quota
may safely be released. No such hook is installed by PHOENIX.
