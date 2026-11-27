# Preserve ledger immutability on Entity Merge collisions and guard cross-phone merges

## Status

accepted

## Context

ReconcileX supports automatic **Entity Merge** across `PAN_HASH` and verified `MOBILE_HASH` identifiers while enforcing `UNIQUE (canonical_user_id, source_type, natural_key)` on `accounts` and strict append-only triggers on `ledger_entries`. Merging two **Canonical Users** who each hold an account with the same `(source_type, natural_key)`—or merging two distinct taxpayers sharing a family phone number—risks either mutating immutable ledger history or corrupting user isolation.

## Decision

1. **Copy-and-archive account collisions**: When both `source_user_id` and `target_user_id` hold the same `(source_type, natural_key)`, `target_acc` remains canonical on `target_user_id`, missing `ledger_transactions` (deduplicated by `idempotency_key`, skipping duplicate `OPENING_ANCHOR`) and their `ledger_entries` are copied to `target_acc` as new immutable rows, `source_acc` remains untouched on the soft-merged `source_user_id` for audit, and the collision is recorded in `entity_merge_events.metadata.collided_accounts`.
2. **PAN-conflict and unverified-mobile safeguards**: Only FIP-verified `MOBILE_HASH` values from AA (`DEPOSIT`, `EQUITIES`) or `LIABILITY` primary holders (`Holder[0]`) trigger **Entity Merge** (`CAS` `investor_info.mobile` is stored unverified), `EMAIL_HASH` never triggers merges, and any merge candidate pairing two distinct verified `PAN_HASH` values is blocked rather than consolidated.

## Considered Options

- **Repointing `ledger_entries.account_id` on collision**: Rejected because altering historical `account_id` foreign keys violates append-only ledger immutability and requires disabling database triggers during merges.
- **Auto-merging on CAS contact mobile or without PAN-conflict checks**: Rejected because Indian families frequently share an RTA contact mobile across spouses/parents with distinct PANs.

## Consequences

- `ledger_entries` and `ledger_transactions` remain strictly immutable even when overlapping accounts arrive before a **Canonical User** merge.
- Queries against active portfolios must filter `users.status = 'ACTIVE'` (or `canonical_user_id = target_user_id`) so archived collision accounts on `MERGED` users are excluded from live balances and XIRR.
