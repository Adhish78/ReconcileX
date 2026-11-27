# `codereverser/casparser` JSON Schema & Indian MF Transaction Conventions

**Ticket:** #3 (`wayfinder:research`)  
**Scope:** Exact JSON schema emitted by [`codereverser/casparser`](https://github.com/codereverser/casparser) (`v1.4.1`, `main` at `75d39aa`) for CAMS/KFintech Consolidated Account Statements (`CASData`), `TransactionType` enum values, unit/amount sign conventions, and unit-conservation invariants for `internal/cas` and the unit-checksum validator.

---

## 1. Output Models & Serialization Overview

`casparser.read_cas_pdf(..., output="json")` serializes the parsed Pydantic v2 model via [`cas2json`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/utils.py) (`data.model_dump_json(by_alias=True)`):

- **CAMS / KFintech (`FileType.CAMS` | `FileType.KFINTECH`)** returns **`CASData`** ([`casparser/types.py`](https://github.com/codereverser/casparser/blob/main/casparser/types.py), [`schema/CASData.schema.json`](https://github.com/codereverser/casparser/blob/main/schema/CASData.schema.json)). This is the transaction-bearing RTA CAS model (`DETAILED` or `SUMMARY`).
- **NSDL / CDSL (`FileType.NSDL` | `FileType.CDSL`)** returns **`NSDLCASData`** ([`casparser/types.py`](https://github.com/codereverser/casparser/blob/main/casparser/types.py), [`schema/NSDLCASData.schema.json`](https://github.com/codereverser/casparser/blob/main/schema/NSDLCASData.schema.json)), which is **holdings-only** (`accounts[]` + optional `nps`) and carries **no `folios` or `transactions`**.

### JSON Wire-Type Conventions

1. **Decimals as JSON Strings**: Every `Decimal` field serializes as a JSON string matching `^(?!^[-+.]*$)[+-]?0*\d*\.?\d*$` (e.g., `"4999.75"`, `"-500.00"`, `"33.412"`), preserving exact arbitrary-precision arithmetic without IEEE-754 float drift ([`schema/CASData.schema.json`](https://github.com/codereverser/casparser/blob/main/schema/CASData.schema.json)).
2. **Dates**:
   - `statement_period.from` and `statement_period.to` are strings kept in the CAS header's `DD-MMM-YYYY` format (e.g., `"01-Apr-2024"`), or `""` if unparsed ([`casparser/parsers/cams_detailed.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/cams_detailed.py), [`README.md`](https://github.com/codereverser/casparser/blob/main/README.md)).
   - `valuation.date` and `transactions[].date` are typed `Union[date, str]`; parsers parse them to `datetime.date`, which Pydantic v2 serializes to ISO-8601 `"YYYY-MM-DD"` (fallback `"1970-01-01"` when missing).
3. **Enums as String Names**: `CASData.model_config` sets `use_enum_values=True` and `populate_by_name=True`, so `file_type`, `cas_type`, and `transactions[].type` serialize as their enum names (e.g., `"CAMS"`, `"DETAILED"`, `"PURCHASE_SIP"`).

---

## 2. Exact `CASData` JSON Schema

Source: [`casparser/types.py`](https://github.com/codereverser/casparser/blob/main/casparser/types.py), [`casparser/enums.py`](https://github.com/codereverser/casparser/blob/main/casparser/enums.py), [`schema/CASData.schema.json`](https://github.com/codereverser/casparser/blob/main/schema/CASData.schema.json).

### 2.1 Top-Level Keys (`CASData`)

| Key | JSON Type | Required | Description / Allowed Values |
| --- | --- | --- | --- |
| `statement_period` | `object` (`StatementPeriod`) | Yes | `{"from": string, "to": string}`. Note `from_` is aliased to `"from"` via `by_alias=True`. |
| `file_type` | `string` (`FileType`) | Yes | `"UNKNOWN"`, `"CAMS"`, `"KFINTECH"`, `"CDSL"`, `"NSDL"` (for `CASData`: `"CAMS"` or `"KFINTECH"`). |
| `cas_type` | `string` (`CASFileType`) | Yes | `"UNKNOWN"`, `"SUMMARY"`, `"DETAILED"`. |
| `investor_info` | `object` (`InvestorInfo`) | Yes | Investor metadata block from page 1. |
| `folios` | `array<Folio>` | Yes | List of mutual fund folios grouped by `(amc, folio)` in `DETAILED` (by `folio` in `SUMMARY`). |
| `parse_warnings` | `array<string>` | Default `[]` | Non-fatal parser warnings (abandoned scheme header regions or running-balance discontinuities). |

### 2.2 `investor_info` (`InvestorInfo`)

Extracted from the top-left column of page 1 in [`casparser/parsers/_investor.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/_investor.py). Missing `name` raises `CASParseError`.

| Field | JSON Type | Required | Notes |
| --- | --- | --- | --- |
| `name` | `string` | Yes | First line after `Email Id:`. |
| `email` | `string` | Yes | Extracted from `Email Id: <email>` (may be `""` if unmatched). |
| `address` | `string` | Yes | Lines between `name` and `Mobile:`, joined by `"\n"` (`Phone Off:` lines stripped). |
| `mobile` | `string` | Yes | Extracted from `Mobile: <mobile>` (may include leading `+`, e.g., `"+919999999999"`). |

### 2.3 `folios[]` (`Folio`)

| Field | JSON Type | Required / Default | Notes |
| --- | --- | --- | --- |
| `folio` | `string` | Yes | Digits with optional sub-account suffix preserving spaces around `/` (e.g., `"12345678 / 90"`). |
| `amc` | `string` | Yes | AMC header line (e.g., `"HDFC Mutual Fund"`), or `"UNKNOWN"` if missing. |
| `name` | `string \| null` | Default `null` | First/primary holder name printed after `Folio No:` on newer `DETAILED` templates; `null` on older templates or `SUMMARY`. |
| `PAN` | `string \| null` | Default `null` | 10-char PAN (`[A-Z]{5}\d{4}[A-Z]`). **Caveat:** `cams_detailed.py` and `cams_summary.py` pass `""` (empty string) rather than `null` when omitted on the line. |
| `KYC` | `string \| null` | Default `null` | `"OK"`, `"NOT OK"`, or `null`. |
| `PANKYC` | `string \| null` | Default `null` | `"OK"`, `"NOT OK"`, or `null`. |
| `schemes` | `array<Scheme>` | Yes | Schemes held under this `(amc, folio)`. |

### 2.4 `folios[].schemes[]` (`Scheme`)

| Field | JSON Type | Required / Default | Notes |
| --- | --- | --- | --- |
| `scheme` | `string` | Yes | Normalized scheme name (stripped of `(formerly ...)`, `(erstwhile ...)`, `(Demat ...)`, `(Advisor: ...)`, inline ISIN). |
| `advisor` | `string \| null` | Default `null` | Distributor `ARN-xxxx` or RIA `INAxxxx` code if present; `null` in `SUMMARY` or direct plans without advisor tag. |
| `rta_code` | `string` | Yes | Per-RTA scheme code preceding `-` in the scheme line (e.g., `"H1234"`, `"128TSGPG"`). |
| `rta` | `string` | Yes | Registrar token (`"CAMS"`, `"KFINTECH"`, `"KFIN"`, `"KARVY"`, `"FTAMIL"`, etc.; defaults to `"CAMS"`). |
| `type` | `string \| null` | Default `null` | Resolved via `casparser-isin` (`"EQUITY"`, `"DEBT"`, etc.); parsers assign `"N/A"` when unresolved. |
| `isin` | `string \| null` | Default `null` | 12-char `INF...` ISIN resolved from header or `casparser-isin`. |
| `amfi` | `string \| null` | Default `null` | AMFI scheme code string resolved from `casparser-isin` (e.g., `"118834"`). |
| `nominees` | `array<string>` | Default `[]` | Up to 3 nominee names from `Nominee 1/2/3:` header lines (`[]` in `SUMMARY`). |
| `open` | `string` (Decimal) | Yes | Opening unit balance (`Opening Unit Balance:` in `DETAILED`; equals `balance` in `SUMMARY`). |
| `close` | `string` (Decimal) | Yes | Printed closing unit balance (`Closing Unit Balance:` in `DETAILED`; equals `balance` in `SUMMARY`). |
| `close_calculated` | `string` (Decimal) | Yes | `open + sum(t.units for t in transactions if t.units is not None)`. |
| `valuation` | `object` (`SchemeValuation`) | Yes | Closing valuation summary. |
| `transactions` | `array<TransactionData>` | Yes | Chronological transaction rows (`[]` in `SUMMARY`). |

### 2.5 `schemes[].valuation` (`SchemeValuation`)

| Field | JSON Type | Required / Default | Notes |
| --- | --- | --- | --- |
| `date` | `string` | Yes | ISO `"YYYY-MM-DD"` (from `NAV on` / `Valuation on` in `DETAILED`, or `NAV Date` / statement date in `SUMMARY`; fallback `"1970-01-01"`). |
| `nav` | `string` (Decimal) | Yes | Closing NAV in INR. |
| `cost` | `string \| null` (Decimal) | Default `null` | From `Total Cost Value` (`DETAILED`) or `Cost Value` (`SUMMARY`), if printed. |
| `value` | `string` (Decimal) | Yes | Market value in INR (`Valuation on` / `Market Value on`). |

### 2.6 `schemes[].transactions[]` (`TransactionData`)

| Field | JSON Type | Required / Default | Notes |
| --- | --- | --- | --- |
| `date` | `string` | Yes | ISO `"YYYY-MM-DD"`. |
| `description` | `string` | Yes | Transaction column text (wrapped continuation lines within `10.0pt` baseline gap merged with `" "`). |
| `amount` | `string \| null` (Decimal) | Default `null` | Cash value in INR. `null` on informational `MISC` marker rows (e.g., `***Registration of Nominee***`) or restatement rows. |
| `units` | `string \| null` (Decimal) | Default `null` | Signed unit delta. **`null` on tax rows (`STT_TAX`, `STAMP_DUTY_TAX`, `TDS_TAX`) and `MISC` rows.** |
| `nav` | `string \| null` (Decimal) | Default `null` | Printed `Price` / `NAV`, or derived `round(amount / units, 4)` when `Price` is absent and `amount` & non-zero `units` exist. `null` on tax/marker rows. |
| `nav_derived` | `boolean` | Default `false` | `true` iff `nav` was derived as `amount / units` (4 decimal places) because no `Price`/`NAV` cell was printed. |
| `balance` | `string \| null` (Decimal) | Default `null` | Printed running `Unit Balance` after the row. **`null` on tax rows** and `***` marker rows; populated on unit-bearing rows and balance-restatement `MISC` rows. |
| `type` | `string` (`TransactionType`) | Yes | Classified by `get_transaction_type(description, units)`. |
| `dividend_rate` | `string \| null` (Decimal) | Default `null` | Per-unit INR dividend extracted via `(?:div.\|dividend\|idcw).*?@\s*Rs\.\s*([\d.]+)` on `DIVIDEND_*` rows; `null` otherwise. |
| `gift_folio` | `string \| null` | Default `null` | Counterparty folio digits extracted via `Folio\s+No\s*[:.]\s*(\d+)` on `GIFT_IN` / `GIFT_OUT`; `null` otherwise. |

> **Subtle edge case in `_sort_transactions`:** When `read_cas_pdf(..., sort_transactions=True)` (the default) encounters out-of-order transaction dates within a scheme, [`_sort_transactions`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/__init__.py) sorts by `txn.date` and overwrites `txn.balance` on **every** transaction (including tax rows, using `txn.units or 0`). When dates are already sorted, `txn.balance` stays `null` on tax rows.

---

## 3. `TransactionType` Enum & Sign Conventions

Source: [`casparser/enums.py`](https://github.com/codereverser/casparser/blob/main/casparser/enums.py), [`casparser/parsers/_classify.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/_classify.py), [`casparser/parsers/cams_detailed.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/cams_detailed.py).

Numbers enclosed in parentheses `(1,234.56)` or prefixed with `-` in the PDF are parsed as negative `Decimal` values by `_decimal()`. Classification priority in `get_transaction_type(description, units)` is strictly ordered as follows:

| `TransactionType` | Classification Rule | `units` Sign / Value | `amount` Sign / Value | `nav` | `balance` | Accounting / Conservation Notes |
| --- | --- | --- | --- | --- | --- | --- |
| `DIVIDEND_REINVEST` | Matches `DIVIDEND_RE` (`@ Rs. <rate>`) **and** `"reinvest"` | `> 0` (typically) | `> 0` (net reinvested) | Populated | Populated | Checked **before** `units` sign; adds units; carries `dividend_rate`. |
| `DIVIDEND_PAYOUT` | Matches `DIVIDEND_RE` (`@ Rs. <rate>`) **without** `"reinvest"` | `null` or `0` (cash payout) | `> 0` | `null` | `null` (or printed) | Carries `dividend_rate`; does not add units. If description lacks `@ Rs. ...` and `units` is `null`, falls back to `MISC`. |
| `STT_TAX` | `units is None` and `"stt"` in description | `null` | `> 0` (positive charge) | `null` | `null` | Securities Transaction Tax on equity redemptions/switch-outs; **not** deductible in Sec. 112A / 48 gains. |
| `STAMP_DUTY_TAX` | `units is None` and `"stamp"` in description | `null` | `> 0` (positive charge) | `null` | `null` | 0.005% buy-side stamp duty; folded into cost of acquisition (Sec. 55) in [`gains.py`](https://github.com/codereverser/casparser/blob/main/casparser/analysis/gains.py). |
| `TDS_TAX` | `units is None` and `"tds"` in description | `null` | `> 0` (positive withholding) | `null` | `null` | Tax Deducted at Source (e.g., NRI redemptions / IDCW). |
| `MISC` | `units is None` and none of `stt`/`stamp`/`tds` | `null` | `null` (or value if printed) | `null` | `null` or printed | Covers `***` informational rows (nominee/KYC/address) and Transmission/Transformation balance restatements. |
| `GIFT_IN` | `units > 0` and `"gift"` in description | `> 0` | `> 0` (or `null`) | Populated | Populated | Populates `gift_folio` (source folio); excluded from capital gains (`gains.py`). |
| `SWITCH_IN_MERGER` | `units > 0` and (`"switch"` or STP) and `"merger"` | `> 0` | `> 0` | Populated | Populated | Scheme merger / consolidation inflow. |
| `SWITCH_IN` | `units > 0` and (`"switch"` or `STP_RE`) | `> 0` | `> 0` | Populated | Populated | Includes CAMS `"Systematic Transfer Plan Switch In"` and KFintech `"S T P In"`. |
| `SEGREGATION` | `units > 0` and `"segregat"` in description | `> 0` | `0` or `null` | `0` or `null` | Populated | Side-pocketed portfolio unit allotment. |
| `PURCHASE_SIP` | `units > 0` and (`"sip"`, `"systematic"`, `instal+ment`, `sys.+?invest`) | `> 0` | `> 0` | Populated | Populated | SIP instalment purchase. |
| `PURCHASE` | `units > 0` (fallback) | `> 0` | `> 0` | Populated | Populated | Lump-sum / NFO / general purchase. |
| `GIFT_OUT` | `units < 0` and `"gift"` in description | `< 0` | `< 0` (or `null`) | Populated | Populated | Populates `gift_folio` (destination folio); excluded from capital gains (`gains.py`). |
| `REVERSAL` | `units < 0` and matches `reversal\|rejection\|dishonoured\|mismatch\|insufficient balance\|payment not received` | `< 0` (usually) | `< 0` (usually) | Populated | Populated | **Note:** If a KFin `Reversed` row prints cosmetic parens `(units)` while increasing the running balance, `_apply_balance_sign_fix` flips `units` and `amount` to positive and re-runs `get_transaction_type`. |
| `SWITCH_OUT_MERGER` | `units < 0` and (`"switch"` or STP) and `"merger"` | `< 0` | `< 0` | Populated | Populated | Scheme merger / consolidation outflow. |
| `SWITCH_OUT` | `units < 0` and (`"switch"` or `STP_RE`) | `< 0` | `< 0` | Populated | Populated | Includes STP outflows. |
| `REDEMPTION` | `units < 0` (fallback) | `< 0` | `< 0` | Populated | Populated | Regular redemption / SWP withdrawal. |
| `UNKNOWN` | `units == 0` (and not matched by `DIVIDEND_RE`) | `0` | Any | Any | Any | Rare fallback when printed units equal literal `0`. |

---

## 4. Unit Conservation & Checksum Behavior

In real `casparser` outputs (`DETAILED` CAMS/KFintech statements), unit conservation is governed by three mechanisms implemented in [`cams_detailed.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/cams_detailed.py) and asserted in [`tests/_assertions.py`](https://github.com/codereverser/casparser/blob/main/tests/_assertions.py):

1. **Tax and Marker Lines Have `units: null` (Never `0`)**:
   - Rows for `STAMP_DUTY_TAX`, `STT_TAX`, `TDS_TAX`, and `MISC` do not print units in the CAS table, so `cams_detailed.py` emits `"units": null`, `"nav": null`, and `"balance": null` (except balance-restatement `MISC` rows, which keep their printed `balance`).
   - Any unit-checksum validator **must treat `units: null` as `0` (skip)** when summing units, while keeping `amount` for cash-flow / cost-basis accounting (`STAMP_DUTY_TAX` adds to buy cost; `STT_TAX` is tracked separately; `amount` on purchase rows in CAMS/KFin CAS is **net of stamp duty**, e.g. ₹24,998.75 purchase + ₹1.25 stamp duty = ₹25,000.00 gross cash outflow).
2. **Per-Row Running Balance Sign Repair (`_apply_balance_sign_fix`)**:
   - Walks from `prev_balance = scheme.open` with tolerance `0.005`.
   - If `abs((prev_balance + units) - balance) > 0.005` **and** `abs((prev_balance - units) - balance) <= 0.005`, `casparser` flips `t.units = -units`, `t.amount = -amount` (if present), and reclassifies `t.type` via `get_transaction_type`. It then recomputes `scheme.close_calculated`.
3. **Scheme-Level Conservation Equation & Tolerances**:
   $$\text{open} + \sum_{t \in \text{transactions},\; t.\text{units} \neq \text{null}} t.\text{units} = \text{close\_calculated} \approx \text{close}$$
   - **Parser reconciliation (`_reconcile_balances`)**: Uses tolerance `Decimal("0.005")`. Checks every row with non-null `t.balance` (`abs(running - printed) <= 0.005`), resyncing on mismatch and appending a string to `parse_warnings`, then checks `abs(running - scheme.close) <= 0.005`.
   - **Test-suite invariant (`assert_scheme_transaction_units_close`)**: Enforces `abs(scheme.open + sum_units - scheme.close) <= Decimal("0.001")` (3-decimal CAS unit precision).
   - **Valuation cross-check (`assert_scheme_valuation_arithmetic`)**: Checks `scheme.close * scheme.valuation.nav ≈ scheme.valuation.value` within `0.5%` relative tolerance (when `close == 0`, `value == 0`).

---

## 5. Example `CASData` JSON Snippet

Adapted from [`README.md`](https://github.com/codereverser/casparser/blob/main/README.md) and [`tests/test_cams_units.py`](https://github.com/codereverser/casparser/blob/main/tests/test_cams_units.py) to reflect exact `cas2json` serialization (`by_alias=True`, all `TransactionData` fields including `nav_derived` and `gift_folio`):

```json
{
  "statement_period": {
    "from": "01-Apr-2024",
    "to": "31-Mar-2025"
  },
  "file_type": "CAMS",
  "cas_type": "DETAILED",
  "investor_info": {
    "name": "JOHN DOE",
    "email": "john@example.com",
    "address": "1, MAIN STREET\nBENGALURU 560001",
    "mobile": "+919999999999"
  },
  "folios": [
    {
      "folio": "12345678 / 90",
      "amc": "HDFC Mutual Fund",
      "name": "JOHN DOE",
      "PAN": "ABCDE1234F",
      "KYC": "OK",
      "PANKYC": "OK",
      "schemes": [
        {
          "scheme": "HDFC Flexi Cap Fund - Direct Plan - Growth",
          "advisor": "ARN-12345",
          "rta_code": "H1234",
          "rta": "CAMS",
          "type": "EQUITY",
          "isin": "INF179K01VY8",
          "amfi": "118834",
          "nominees": ["JANE DOE"],
          "open": "0.000",
          "close": "23.412",
          "close_calculated": "23.412",
          "valuation": {
            "date": "2025-03-31",
            "nav": "165.4100",
            "cost": "3503.07",
            "value": "3872.58"
          },
          "transactions": [
            {
              "date": "2024-04-02",
              "description": "SIP Purchase - Instalment 1/12",
              "amount": "4999.75",
              "units": "33.412",
              "nav": "149.6400",
              "nav_derived": false,
              "balance": "33.412",
              "type": "PURCHASE_SIP",
              "dividend_rate": null,
              "gift_folio": null
            },
            {
              "date": "2024-04-02",
              "description": "*** Stamp Duty ***",
              "amount": "0.25",
              "units": null,
              "nav": null,
              "nav_derived": false,
              "balance": null,
              "type": "STAMP_DUTY_TAX",
              "dividend_rate": null,
              "gift_folio": null
            },
            {
              "date": "2025-02-10",
              "description": "Redemption - ECS",
              "amount": "-1600.00",
              "units": "-10.000",
              "nav": "160.0000",
              "nav_derived": false,
              "balance": "23.412",
              "type": "REDEMPTION",
              "dividend_rate": null,
              "gift_folio": null
            },
            {
              "date": "2025-02-10",
              "description": "*** STT Paid ***",
              "amount": "0.02",
              "units": null,
              "nav": null,
              "nav_derived": false,
              "balance": null,
              "type": "STT_TAX",
              "dividend_rate": null,
              "gift_folio": null
            }
          ]
        }
      ]
    }
  ],
  "parse_warnings": []
}
```

---

## 6. Primary Sources

- [`codereverser/casparser/casparser/types.py`](https://github.com/codereverser/casparser/blob/main/casparser/types.py) — Pydantic v2 data models (`CASData`, `StatementPeriod`, `InvestorInfo`, `Folio`, `Scheme`, `SchemeValuation`, `TransactionData`, `NSDLCASData`).
- [`codereverser/casparser/casparser/enums.py`](https://github.com/codereverser/casparser/blob/main/casparser/enums.py) — `FileType`, `CASFileType`, `FundType`, `GainType`, and `TransactionType` definitions.
- [`codereverser/casparser/schema/CASData.schema.json`](https://github.com/codereverser/casparser/blob/main/schema/CASData.schema.json) — Auto-generated Draft 2020-12 JSON Schema for `CASData`.
- [`codereverser/casparser/casparser/parsers/_classify.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/_classify.py) — `get_transaction_type`, `DIVIDEND_RE`, `STP_RE`, `GIFT_FOLIO_RE`.
- [`codereverser/casparser/casparser/parsers/cams_detailed.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/cams_detailed.py) — Column-based parser, `_decimal`, `nav_derived`, `_apply_balance_sign_fix`, `_reconcile_balances`, `parse_warnings`.
- [`codereverser/casparser/casparser/parsers/cams_summary.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/cams_summary.py) — Summary statement parser (`transactions: []`, `open == close`).
- [`codereverser/casparser/casparser/parsers/__init__.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/__init__.py) — `read_cas_pdf` dispatcher and `_sort_transactions`.
- [`codereverser/casparser/casparser/parsers/_investor.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/_investor.py) — `extract_cams_kfin_investor` and `extract_nsdl_cdsl_investor`.
- [`codereverser/casparser/casparser/parsers/utils.py`](https://github.com/codereverser/casparser/blob/main/casparser/parsers/utils.py) — `cas2json` (`model_dump_json(by_alias=True)`).
- [`codereverser/casparser/tests/_assertions.py`](https://github.com/codereverser/casparser/blob/main/tests/_assertions.py) — `assert_scheme_transaction_units_close` (`0.001` tolerance) and `assert_scheme_valuation_arithmetic` (`0.5%` relative tolerance).
- [`codereverser/casparser/casparser/analysis/gains.py`](https://github.com/codereverser/casparser/blob/main/casparser/analysis/gains.py) — Indian tax treatment of `STAMP_DUTY_TAX`, `STT_TAX`, `TDS_TAX`, `GIFT_IN`/`GIFT_OUT`, and `SEGREGATION`.
- [`codereverser/casparser/README.md`](https://github.com/codereverser/casparser/blob/main/README.md) — Documented schema shape and sample output.
