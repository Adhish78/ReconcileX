# Enforce immutable double-entry invariants and user-agnostic ledger tables

## Status

accepted

## Context

ReconcileX requires strict zero-sum double-entry accounting across multi-source ingestions while also supporting cross-phone **Entity Merge** consolidation and post-ingestion reclassification of `UNCATEGORIZED_REVIEW` **Narrations**.

## Decision

`ledger_entries` stores signed monetary deltas (`NUMERIC(18,4)`) and optional unit deltas (`NUMERIC(18,6)`)—enforcing `CHECK (amount <> 0 OR (units IS NOT NULL AND units <> 0))` so zero-cash unit events (`BONUS`, `SPLIT`, `SEGREGATION`, costless `OPENING_ANCHOR`) record exact unit deltas without synthetic `₹0.0001` cash—guarded by a `DEFERRABLE INITIALLY DEFERRED` constraint trigger enforcing `SUM(amount) = 0` per `transaction_id` and a strict append-only trigger blocking `UPDATE` and `DELETE`. `canonical_user_id` is stored on `accounts` rather than `ledger_transactions`, and `ledger_transactions` permits `UPDATE` solely on `(category, category_updated_at)` so entity merges and human review never mutate accounting legs.

## Considered Options

- **Denormalizing `canonical_user_id` onto `ledger_transactions`**: Rejected because every **Entity Merge** would require rewriting historical transaction rows, conflicting with ledger immutability.
- **Application-only zero-sum validation**: Rejected because bugs, concurrent workers, or manual SQL fixtures could bypass accounting conservation without database-level enforcement.

## Consequences

- Cross-user merges only reassign `accounts` and `linked_identities` while soft-marking the superseded `users` row (`status = 'MERGED'`).
- Live account balances and unit totals are always aggregated directly from `ledger_entries` via `(account_id, coa_account) INCLUDE (amount, units)`, eliminating cached-balance drift.
