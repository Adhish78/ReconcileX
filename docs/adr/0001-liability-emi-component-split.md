# Split EMI repayments into principal, interest, and fee components

## Status

accepted

## Context

ReconcileX reconciles each account's computed ledger balance against the institution's reported balance (`|computed - reported| > ₹0.01`). For loans (`HOME_LOAN`, `AUTO_LOAN`), the reported `outstandingBalance` is unpaid principal, whereas an EMI debit from a bank account equals `principal + interest + fees`.

## Decision

`EMI_REPAYMENT` transactions in the internal `LIABILITY` ingestion contract carry explicit `principalComponent`, `interestComponent`, and `feeComponent` fields (whose sum must equal `amount`, defaulting to `100%` principal if omitted). Ingestion posts only `principalComponent` as a reduction to `LIABILITY:LOAN` and routes `interestComponent` and `feeComponent` to `EXPENSE:LOAN_INTEREST` / `EQUITY:EXTERNAL_CLEARING`, keeping `LIABILITY:LOAN` equal to exact unpaid principal.

## Considered Options

- **Separate accrual transactions**: Requiring every loan statement to emit paired `INTEREST_ACCRUAL` and gross `EMI_REPAYMENT` rows, rejected because Indian loan schedules and synthetic fixtures naturally state each EMI's principal/interest breakup on the installment row itself.
- **Gross EMI posting**: Posting the full EMI amount directly against `LIABILITY:LOAN`, rejected because it immediately creates false-positive balance-drift alerts equal to cumulative interest paid.

## Consequences

- `LIABILITY:LOAN` tracks exact remaining loan principal and reconciles directly against `Summary.outstandingBalance` with zero false-positive drift.
- Ticket #7's Chart of Accounts enum must include an interest/fee expense account (`EXPENSE:LOAN_INTEREST`) alongside `EXPENSE:UPI_SPEND` and `EXPENSE:TAX_STAMP_DUTY`.
