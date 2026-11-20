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

While a `client_renew` order remains `processing`, a subsequent payment for the
same normalized email is rejected. Reseller client mutations, participating
quota checks and automatic renewals are also blocked while the affected client
remains mapped to that reseller. These safeguards do not fence admin actions,
external runtime changes, or changes to ownership outside the participating
paths. Review those separately during reconciliation.

Public checkout checks the current reseller allocation and unresolved creation
or renewal markers before issuing an order. This check does **not** reserve
quota: another operation or order can consume capacity before payment arrives.
The webhook checks again and may reject an already-paid order, which then needs
provider reconciliation or refund. Operators should not treat order creation as
a guarantee that capacity has been reserved.

For a priced pending client renewal or reseller payment, callbacks and manual
processing must include a positive amount matching the recorded amount (within
the currency rounding tolerance). An omitted, zero, negative or mismatching
amount is rejected. Existing integrations that previously omitted the amount
must send it. The signed callback and matching amount alone do not independently
verify payment with a gateway; secure and verify the gateway integration.

New public checkout orders retain the configured currency at order creation,
so later settings changes do not alter their amount-comparison precision.
A reseller deposit whose recorded currency differs from the current wallet
currency is not credited automatically; reconcile or refund it without assuming
an exchange rate. Orders created before currency snapshots have no reliable
currency evidence and continue to use the current setting for compatibility;
operators must inspect their gateway records before processing them after a
currency change. The callback does not itself carry verified currency evidence.

Checkout treats a zero per-GB or per-month price as disabling that particular
purchase option, not as a free add-on to a paid option. Configure a positive
price for every option intended for sale. Previously created pending orders
retain their original terms and require review if they included a zero-priced
component unexpectedly.
