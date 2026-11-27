# Use a deterministic two-tier narration categorizer without P2P guessing

## Status

accepted

## Context

ReconcileX must classify 10,000+ Indian banking and card narrations (`UPI/DR`, `ACH/DR`, `NEFT/CR`, `IMPS`, `CARD`) reproducibly in a zero-setup showcase without external LLM/ML dependencies, while keeping investment/debt transfers separate from consumption expenses and surfacing ambiguous entries for review.

## Decision

Narrations pass through a deterministic 3-step normalization pipeline followed by a static, boundary-guarded Aho-Corasick Tier-1 keyword matcher (resolved by category precedence `TRANSFER:INVESTMENT > TRANSFER:CARD_PAYMENT > TRANSFER:LOAN_EMI > INCOME:* > EXPENSE:* > TRANSFER:SELF/P2P`, then longest keyword) and 6 ordered Tier-2 `(rail, direction, token)` fallback rules. All remaining unmatched narrations—including unrecognized `UPI/DR` peer VPAs—fall through to `UNCATEGORIZED_REVIEW` rather than being guessed as `TRANSFER:P2P`.

## Considered Options

- **Default unmatched `UPI/DR` to `TRANSFER:P2P`**: Rejected because peer-looking VPAs often belong to unindexed local merchants, and greedy fallback would starve the `UNCATEGORIZED_REVIEW` queue of realistic items.
- **Probabilistic / ML / external LLM classification**: Rejected because it violates zero-setup offline determinism, latency predictability in benchmarks, and auditability.

## Consequences

- Categorization output is 100% deterministic, benchmarkable (`V1 -> V2`), and auditable across all three personas.
- `TRANSFER:*` categories are cleanly excluded from pure consumption spending while `ledger_transactions.category` remains distinct from the double-entry Chart of Accounts.
