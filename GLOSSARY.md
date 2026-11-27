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

**Fee Component**:
The ancillary charge portion (such as late or processing fees) of an **EMI Repayment** recognized as an expense rather than a reduction of loan principal.
_Avoid_: Penalty fee, admin charge

**Financial Account**:
A single reconcilable bank deposit, mutual fund scheme holding, equity demat account, or **Liability Account** owned by a **Canonical User** and balanced independently on the ledger.
_Avoid_: Sub-account, wallet, container

**Consolidated Account Statement (CAS)**:
A standardized CAMS or KFintech mutual fund statement reporting **Folio** unit balances, historical **Ledger Transactions**, and institution valuations.
_Avoid_: MF report, RTA dump

**Folio**:
An investor's mutual fund account identifier with a Registrar and Transfer Agent (RTA) that groups one or more scheme holdings under a **Consolidated Account Statement (CAS)**.
_Avoid_: MF account, scheme account

**Account Aggregator (AA)**:
A ReBIT-standardized consent-based data network delivering institution-reported bank deposit and demat equity holdings, balances, and **Narrations**.
_Avoid_: Open banking feed, FIP scraper

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

**Opening Anchor**:
A synthetic **Ledger Transaction** recorded on first ingestion of a partial statement window to establish the pre-window starting balance or unit count against **External Clearing** (or as a unit-only **Ledger Entry**).
_Avoid_: Seed balance, backfill row

**Unit Checksum**:
The pre-ledger conservation invariant verifying that a mutual fund statement's opening units plus unit-bearing transaction deltas match its printed closing units, routing failing statements to dead-letter handling.
_Avoid_: Share count check, unit balance alert

**Reconciliation Alert**:
An open, acknowledged, or resolved discrepancy (`BALANCE_DRIFT`, `VALUATION_DRIFT`, or `UNIT_DRIFT`) between a computed ledger or NAV value and an institution-reported figure on an account.
_Avoid_: Drift warning, audit error

**Entity Merge**:
The auditable consolidation of two **Canonical Users** triggered by matching a `PAN_HASH` or verified `MOBILE_HASH`, reassigning accounts and **Linked Identities** while soft-marking the superseded user.
_Avoid_: User deduplication, account linking

**Primary Holder**:
The first-listed account owner on an **Account Aggregator (AA)** or **Liability Account** record whose verified mobile number is trusted for **Entity Merge**, distinct from unverified shared contact numbers on a **Consolidated Account Statement (CAS)**.
_Avoid_: Account owner, contact person

**Account Collision**:
A state during an **Entity Merge** where both the source and surviving **Canonical Users** already hold a **Financial Account** with the same institution key, resolved by copying missing **Ledger Transactions** to the surviving account and archiving the source copy.
_Avoid_: Duplicate account, merge conflict

**Costless Anchor**:
An **Opening Anchor** recorded only in units with zero historical **Cost Basis** when a **Consolidated Account Statement (CAS)** starts mid-history, excluded from **XIRR** return calculations.
_Avoid_: Zero-cost seed, free units

**Corporate Action**:
A zero-cash unit adjustment event—such as a bonus issue, stock split, or scheme segregation—that alters a holding's units without changing its **Cost Basis**.
_Avoid_: Non-cash txn, share adjustment

**Balance Drift**:
A **Reconciliation Alert** raised when a **Financial Account**'s computed ledger balance diverges from its institution-reported balance beyond tolerance.
_Avoid_: Cash mismatch, ledger gap

**Unit Drift**:
A **Reconciliation Alert** raised when an investment holding's computed units diverge from its institution-reported units beyond tolerance.
_Avoid_: Share mismatch, quantity error

**Valuation Drift**:
An informational **Reconciliation Alert** raised when a **Consolidated Account Statement (CAS)**'s reported valuation diverges from its computed **Market Value** beyond tolerance.
_Avoid_: NAV error, price discrepancy

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

**Payment Rail**:
The underlying Indian clearing channel (such as UPI, ACH, NACH, NEFT, IMPS, or card network) extracted from a **Narration** to guide deterministic categorization.
_Avoid_: Transfer mode, network tag

**Internal Transfer**:
A money movement between a **Canonical User**'s own accounts, investments, loan **EMI Repayments**, or card settlements that is excluded from consumption spending totals.
_Avoid_: Contra entry, self-payment

### Valuation, Returns & Showcase

**Cost Basis**:
The historical cumulative monetary outlay recorded on the ledger to acquire an investment holding, kept separate from fluctuating market valuations.
_Avoid_: Invested value, purchase price

**Net Asset Value (NAV)**:
The per-unit daily price of a mutual fund scheme used to compute **Market Value** and **Valuation Drift**.
_Avoid_: Unit price, share rate

**Market Value**:
The current estimated worth of an investment holding computed at read time by multiplying ledger units by the latest **Net Asset Value (NAV)** or reported security price.
_Avoid_: Current value, portfolio worth

**Unrealized P&L**:
The read-time gain or loss on an active holding calculated as **Market Value** minus **Cost Basis**.
_Avoid_: Paper profit, notional return

**Net Worth**:
Total asset balances and **Market Values** across a **Canonical User**'s bank deposits, mutual funds, and equities minus total **Outstanding Balances** on **Liability Accounts**.
_Avoid_: Total wealth, equity value

**XIRR**:
The annualized cash-flow-weighted rate of return computed per scheme or portfolio using net investor cash flows and terminal **Market Value**, yielding `N/A` for single-sign series, sub-day windows, or **Costless Anchors**.
_Avoid_: CAGR, simple return

**Persona**:
A pre-seeded showcase **Canonical User** (`Aarav`, `Priya`, or `Rohan`) embodying a deterministic mix of **Financial Accounts**, **Narrations**, **Reconciliation Alerts**, and **Entity Merge** scenarios.
_Avoid_: Demo user, test account
