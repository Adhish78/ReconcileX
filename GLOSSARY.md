# ReconcileX

A multi-source personal wealth and liability reconciliation domain that tracks bank deposits, mutual funds, equities, and debt obligations on an immutable double-entry ledger.

## Language

### Accounts & Liabilities

**Liability Account**:
A debt obligation (`HOME_LOAN`, `AUTO_LOAN`, or `CREDIT_CARD`) ingested with a non-negative **Outstanding Balance** and reconciled against its corresponding liability balance in the **Chart of Accounts**.
_Avoid_: Debt account, negative asset

**Outstanding Balance**:
The total unpaid principal on a loan or the total billed and unbilled amount owed on a credit card as of a statement or fetch timestamp.
_Avoid_: Current balance, amount due

**Total Amount Due**:
The billed statement balance on a credit card payable by its due date, distinct from the real-time **Outstanding Balance** which also includes unbilled **Purchases**.
_Avoid_: Statement outstanding, minimum due

**EMI Repayment**:
A periodic loan installment settled by the borrower, explicitly split into **Principal Component**, **Interest Component**, and **Fee Component** so unpaid principal reconciles without interest-induced drift.
_Avoid_: Loan debit, installment

**Principal Component**:
The portion of an **EMI Repayment** that directly reduces a loan's **Outstanding Balance**.
_Avoid_: Principal payment, net repayment

**Interest Component**:
The borrowing-cost portion of an **EMI Repayment** recognized as an expense rather than a reduction of loan principal.
_Avoid_: Finance charge, interest debit

**Chart of Accounts**:
The minimal set of double-entry ledger accounts used to guarantee zero-sum balancing across assets, liabilities, clearing equity, income, and expenses.
_Avoid_: Category list, spending buckets

**External Clearing**:
The `EQUITY:EXTERNAL_CLEARING` balancing account in the **Chart of Accounts** that lets each financial container balance independently without fragile cross-source matching.
_Avoid_: Suspense account, offset bucket

### Identity, Ledger & Reconciliation

**Canonical User**:
A deduplicated person record owning **Linked Identities** and accounts, consolidated via **Entity Merge** when shared verified identifiers are discovered.
_Avoid_: Customer, login, profile

**Linked Identity**:
A hashed external identifier (`PAN_HASH`, `MOBILE_HASH`, or `EMAIL_HASH`) bound to a **Canonical User** with verification status and source provenance.
_Avoid_: Credential, auth token, identifier

**Ledger Transaction**:
An atomic financial event on a single account (from AA, CAS, or Liability ingestion, or an `OPENING_ANCHOR`) whose **Ledger Entries** sum to zero.
_Avoid_: Journal batch, posting group

**Ledger Entry**:
An immutable signed monetary and optional unit delta against one **Chart of Accounts** bucket belonging to a **Ledger Transaction**.
_Avoid_: Line item, split, row

**Reconciliation Alert**:
An open, acknowledged, or resolved discrepancy (`BALANCE_DRIFT`, `VALUATION_DRIFT`, or `UNIT_DRIFT`) between a computed ledger or NAV value and an institution-reported figure on an account.
_Avoid_: Drift warning, audit error

**Entity Merge**:
The auditable consolidation of two **Canonical Users** triggered by matching a `PAN_HASH` or verified `MOBILE_HASH`, reassigning accounts and **Linked Identities** while soft-marking the superseded user.
_Avoid_: User deduplication, account linking

### Categorization

**Narration**:
The raw bank or card transaction description string emitted by an institution across rails such as UPI, ACH, NACH, NEFT, IMPS, or card networks.
_Avoid_: Memo, remark, description

**Canonical Category**:
The single analytical classification assigned to a transaction (such as `EXPENSE:FOOD_DINING`, `TRANSFER:INVESTMENT`, or `UNCATEGORIZED_REVIEW`), kept separate from the **Chart of Accounts** entry used for double-entry balancing.
_Avoid_: Ledger account, tag, bucket

**Uncategorized Review**:
The explicit terminal classification (`UNCATEGORIZED_REVIEW`) assigned when deterministic categorization cannot classify a **Narration** with high confidence, routing the entry to human review instead of guessing.
_Avoid_: Misc, unknown, P2P fallback
