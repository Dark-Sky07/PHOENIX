# Client renewal payment reconciliation

Client renewal webhooks can modify client settings, traffic counters and running
services. These effects are not a single database transaction. To avoid applying
an uncertain renewal twice, the order is marked `processing` before any effects
are attempted. A repeated callback for such an order returns an error rather
than applying the renewal again. Successful renewals become `completed`.

A `processing` order may still be executing; do not modify it while a callback
is running. If it remains in this state after a process failure or an error,
inspect the gateway payment, order parameters, client settings, traffic/archive
records, service state and backups before taking any action. The marker alone
does not tell whether none, some or all effects occurred. An administrator must
reconcile the account and payment with those records; **do not blindly retry or
change the status back to pending**, because this can grant a renewal twice.
There is currently no automatic reconciliation or transaction spanning the
runtime and the database. Coordinate any manual correction with a backup and an
audit record.
