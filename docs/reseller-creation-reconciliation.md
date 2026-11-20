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
