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
