# ReBIT Account Aggregator (AA) v2.x FI Data Schemas: `DEPOSIT` and `EQUITIES`

## Executive Summary & Normative Versioning

ReBIT's normative Financial Information (FI) schemas are published as **XML Schema Definitions (XSD)** under the `http://api.rebit.org.in/FISchema/{type}` namespaces, while the AA transport APIs (`AA_2_1_0.yaml` / `FIP_2_1_0.yaml`) use **JSON over HTTPS** (`POST /FI/fetch`). When FIPs or AA/FIU middleware serialize decrypted FI payloads into JSON, keys mirror the XSD element and attribute hierarchy.

Critically, per ReBIT's circular dated January 23, 2025 and Sahamati's v2.0.0 adoption guidelines:

- **`DEPOSIT` (along with `TERM_DEPOSIT` and `RECURRING_DEPOSIT`) migrated to FI Schema `v2.0.0`** (`deposit_v2.0.0.xsd`), with FIP go-live in June–July 2025 and FIU v1.x decommission before July 27, 2025.
- **`EQUITIES` (and all other SEBI-regulated FI types) remain on FI Schema `v1.0.0`** (`equities.xsd`, active since December 29, 2022) even when delivered over ReBIT AA v2.x transport APIs.

---

## 1. Transport Envelope & Decrypted Payload Structure

### 1.1 `POST /FI/fetch` Response Envelope (`FIFetchResponse`)

Defined in ReBIT AA/FIP v2.x OpenAPI specifications (`AA-v2-latest.yaml`, `FIP-v2-latest.yaml`):

| Field | Type | Required | Description |
| :--- | :--- | :--- | :--- |
| `ver` | `string` | Yes | API version (e.g. `"2.0.0"`) |
| `timestamp` | `string` (`date-time`) | Yes | ISO 8601 creation timestamp (e.g. `"2023-06-26T11:39:57.153Z"`) |
| `txnid` | `string` (UUID) | Yes | End-to-end traceability transaction ID |
| `FI` | `array<object>` | Yes | Per-FIP encrypted financial information blocks |
| `FI[].fipID` | `string` | Yes | FIP identifier registered in Central Registry |
| `FI[].KeyMaterial` | `object` | Yes | ECDH key exchange params (`cryptoAlg`: `"ECDH"`, `curve`: `"Curve25519"` \| `"X25519"`, `params`, `DHPublicKey`, `Nonce`, optional `Signature`) |
| `FI[].data` | `array<object>` | Yes | Per-account encrypted payloads |
| `FI[].data[].linkRefNumber` | `string` | Yes | Account link reference assigned during linking |
| `FI[].data[].maskedAccNumber` | `string` | Yes | Masked account/demat number (e.g. `"XXXXXXXX4020"`) |
| `FI[].data[].encryptedFI` | `string` | Yes | End-to-end encrypted FI payload (empty if data not delivered) |

### 1.2 Decrypted `Account` Envelope & JSON Mapping for `internal/aa`

After decrypting `encryptedFI`, the canonical XSD root element is `<Account>` containing optional child elements (`minOccurs="0"`, `maxOccurs="1"` — populated according to consented `consentTypes`: `PROFILE`, `SUMMARY`, `TRANSACTIONS`):

- **`Profile`**: Account holder/KYC metadata (`Holders` -> `Holder[]`).
- **`Summary`**: Current valuation/balances and account metadata.
- **`Transactions`**: Date-bounded (`startDate`, `endDate`) ledger/trade entries (`Transaction[]`).

#### Parsing Considerations for `internal/aa`
1. **Numeric and boolean encoding in JSON**: In ReBIT XSDs, fields typed as `xs:float`, `xs:integer`, `xs:positiveInteger`, and `xs:boolean` originate as XML attributes. XML-to-JSON serializers may emit them as **JSON strings** (e.g. `"90000.00"`, `"9876543210"`) or **JSON numbers/booleans** (`90000.00`). Parsers must accept both string and numeric representations and parse decimal amounts/prices without float precision loss.
2. **Element casing & array normalization**: Native ReBIT-aligned JSON preserves PascalCase XSD element names (`Account`, `Profile`, `Holders`, `Holder`, `Summary`, `Transactions`, `Transaction`, `Investment`, `Holdings`, `Holding`, `PendingTxns`, `PendingTxn`) with camelCase attributes. However, some TSPs/gateways lowercase element keys (`account`, `profile`, `holders`, `holder`, etc.) and emit single-item elements (`Holder`, `Transaction`, `Holding`, `PendingTxn`) as a **JSON object** instead of a 1-element **JSON array** when `Holders.type == "SINGLE"` or `count == 1`. `internal/aa` should normalize both casing styles and object-or-array multiplicity.
3. **v1.x vs v2.0.0 coexistence**: Inspect `Account.version` (`"2.0.0"` vs `"1.0"`/`"1.1"`/`"1.2"`) and support fallback field names where older FIP payloads or mocks use v1.x attributes (noted in Section 2.4).

---

## 2. `DEPOSIT` FI Schema (`deposit_v2.0.0.xsd`)

- **Target Namespace**: `http://api.rebit.org.in/FISchema/deposit`
- **Normative XSD**: `deposit_v2.0.0.xsd`

### 2.1 `Account` Root Attributes

| Attribute | XSD Type | Required | Value / Format / Notes |
| :--- | :--- | :--- | :--- |
| `type` | `xs:string` | Yes | Fixed `"deposit"` |
| `maskedAccNumber` | `xs:string` | Yes | Masked bank account number (e.g. `"XXXXXXXX1234"`) |
| `version` | `aa:version` | Yes | Enum `"2.0.0"` (in v1.x: float `0.0..2.0`, e.g. `"1.1"`) |
| `linkedAccRef` | `xs:string` | Yes | FIP-assigned link reference number |

### 2.2 `Profile.Holders` & `Profile.Holders.Holder[]`

**`Holders`** (1..1 inside `Profile`):

| Attribute | XSD Type | Required | Enum / Notes |
| :--- | :--- | :--- | :--- |
| `type` | `aa:HoldersType` | Yes | `"SINGLE"` \| `"JOINT"` |

**`Holder[]`** (1..unbounded inside `Holders`):

| Attribute | XSD Type | Required | Format / Enum / Notes |
| :--- | :--- | :--- | :--- |
| `name` | `xs:string` | Yes | Account holder full name |
| `dob` | `xs:date` | Yes | ISO date `YYYY-MM-DD` |
| `mobile` | `xs:integer` | Yes | Mobile number digits |
| `ckycRegistered` | `aa:CKYCRegistered` | Yes | `"YES"` \| `"NO"` \| `"NO_DATA_AVAILABLE"` *(new in v2.0.0; replaced `ckycCompliance`)* |
| `nominee` | `aa:HoldingNominee` | No | `"REGISTERED"` \| `"NOT-REGISTERED"` *(became optional in v2.0.0)* |
| `landline` | `xs:string` | No | Landline number |
| `address` | `xs:string` | No | Postal address |
| `email` | `aa:HolderEmail` | No | Regex `[^@]+@[^\.]+\..+` |
| `pan` | `aa:HolderPan` | No | Regex `[a-zA-Z]{5}[0-9]{4}[a-zA-Z]` |

> Note: `deposit_v2.0.0.xsd` defines simpleType `HolderAadhar` (`[0-9]{12}`), but does not attach an `aadhar` attribute to `Holder`.

### 2.3 `Summary` & `Summary.PendingTxns.PendingTxn[]`

| Attribute | XSD Type | Required | Format / Enum / Notes |
| :--- | :--- | :--- | :--- |
| `currentBalance` | `xs:float` | Yes | Available balance *(changed from `string` to `float` in v2.0.0)* |
| `currency` | `xs:string` | Yes | Currency code, e.g. `"INR"` *(became required in v2.0.0)* |
| `balanceDateTime` | `xs:dateTime` | Yes | ISO 8601 timestamp |
| `accountType` | `aa:AccountType` | Yes | `"REGULAR"` \| `"NRE"` \| `"NRO"` \| `"RFC"` *(new in v2.0.0)* |
| `accountSubType` | `aa:AccountSubType` | Yes | `"SAVINGS"` \| `"CURRENT"` *(new in v2.0.0; replaced `Summary.type`)* |
| `branch` | `xs:string` | Yes | Branch location |
| `facility` | `aa:SummaryFacility` | Yes | `"OVERDRAFT"` \| `"CASH_CREDIT"` \| `"NO_FACILITY_GRANTED"` *(required in v2.0.0)* |
| `ifsc` | `aa:SummaryIfsc` | Yes | Regex `[a-zA-Z]{4}[0][0-9]{6}` *(replaced `Summary.ifscCode`)* |
| `micrCode` | `xs:string` | Yes | 9-digit MICR code |
| `openingDate` | `xs:date` | Yes | ISO date `YYYY-MM-DD` |
| `status` | `aa:StatusTypes` | Yes | `"ACTIVE"` \| `"INACTIVE"` |
| `currentODLimit` | `xs:float` | No | Available OD limit *(changed from `string` to `float` in v2.0.0)* |
| `drawingLimit` | `xs:float` | No | Sanctioned OD/CC limit *(changed from `string` to `float` in v2.0.0)* |

**`Summary.PendingTxns`** (`minOccurs="0"`, `maxOccurs="1"` — replaced v1.x `Pending`): contains `PendingTxn[]` (1..unbounded):
- `transactionType` (optional `aa:TransactionType`: `"CREDIT"` | `"DEBIT"`)
- `amount` (required `xs:float`)

### 2.4 `Transactions` & `Transactions.Transaction[]`

**`Transactions`**:
- `startDate` (required `xs:date`, `YYYY-MM-DD`)
- `endDate` (required `xs:date`, `YYYY-MM-DD`)
- `Transaction[]` (`minOccurs="1"`, `maxOccurs="unbounded"` in v2.0.0; was `minOccurs="0"` in v1.x)

**`Transaction[]`**:

| Attribute | XSD Type | Required | Format / Enum / Notes |
| :--- | :--- | :--- | :--- |
| `txnId` | `xs:string` | Yes | Unique transaction ID |
| `type` | `aa:TransactionType` | Yes | `"CREDIT"` \| `"DEBIT"` |
| `mode` | `aa:TransactionMode` | Yes | `"CASH"`, `"UPI"`, `"IMPS"`, `"NEFT"`, `"RTGS"`, `"CARD"`, `"ATM"`, `"CHEQUE"`, `"DEMAND_DRAFT"`, `"AUTO_DEBIT"`, `"INTEREST_CREDIT"`, `"NACH"`, `"ECS"`, `"REMITTANCE"`, `"OTHERS"` |
| `amount` | `xs:float` | Yes | Transaction amount |
| `transactionalBalance` | `xs:float` | Yes | Post-transaction running balance *(new in v2.0.0; replaced `currentBalance`)* |
| `transactionTimestamp` | `xs:dateTime` | Yes | ISO 8601 timestamp |
| `valueDate` | `xs:dateTime` | Yes | ISO 8601 timestamp *(changed from `xs:date` in v1.x to `xs:dateTime` in v2.0.0)* |
| `narration` | `xs:string` | Yes | Transaction description / remark |
| `reference` | `xs:string` | No | Cheque / reference number *(became optional in v2.0.0)* |

#### Summary of `DEPOSIT` v1.x -> v2.0.0 Breaking Changes (`Deposit_v2.0.0_Release_Note.txt`)
- **Added**: `Holder.ckycRegistered`, `Summary.accountType`, `Summary.accountSubType`, `Summary.ifsc`, `Summary.PendingTxns` / `PendingTxn`, `Transaction.transactionalBalance`.
- **Deleted**: `Holder.ckycCompliance`, `Summary.exchangeRate` (`exchgeRate` in v1 XSD), `Summary.type`, `Summary.ifscCode`, `Summary.Pending`, `Transaction.currentBalance`.
- **Requirement changes**: `Summary.currency` and `Summary.facility` became required; `Holder.nominee`, `Summary.currentODLimit`, `Summary.drawingLimit`, `Transaction.reference` became optional; `Transactions.Transaction` `minOccurs` changed from `0` to `1`.
- **Type & enum changes**: `Summary.currentBalance` / `currentODLimit` / `drawingLimit` changed from `string` to `float`; `Transaction.valueDate` changed from `date` to `dateTime`; `Summary.facility` changed from `OD`/`CC` to `OVERDRAFT`/`CASH_CREDIT`/`NO_FACILITY_GRANTED`; `Transaction.mode` expanded from 6 values (`CASH`, `ATM`, `CARD`, `UPI`, `FT`, `OTHERS`) to 15 values.

### 2.5 Representative `DEPOSIT` v2.0.0 JSON Snippet

```json
{
  "Account": {
    "type": "deposit",
    "maskedAccNumber": "XXXXXXXX4020",
    "version": "2.0.0",
    "linkedAccRef": "XXXX-XXXX-XXXX",
    "Profile": {
      "Holders": {
        "type": "SINGLE",
        "Holder": [
          {
            "name": "Asha Kulkarni",
            "dob": "1990-05-14",
            "mobile": "9876543210",
            "nominee": "REGISTERED",
            "address": "12 MG Road, Bengaluru",
            "email": "asha@example.com",
            "pan": "ABCDE1234F",
            "ckycRegistered": "YES"
          }
        ]
      }
    },
    "Summary": {
      "currentBalance": 125430.50,
      "currency": "INR",
      "balanceDateTime": "2025-07-01T10:15:30+05:30",
      "accountType": "REGULAR",
      "accountSubType": "SAVINGS",
      "branch": "MG Road Bengaluru",
      "facility": "NO_FACILITY_GRANTED",
      "ifsc": "HDFC0000123",
      "micrCode": "560240002",
      "openingDate": "2018-03-10",
      "status": "ACTIVE",
      "PendingTxns": {
        "PendingTxn": [
          {
            "transactionType": "DEBIT",
            "amount": 500.00
          }
        ]
      }
    },
    "Transactions": {
      "startDate": "2025-06-01",
      "endDate": "2025-07-01",
      "Transaction": [
        {
          "txnId": "TXN202506150001",
          "type": "DEBIT",
          "mode": "UPI",
          "amount": 1500.00,
          "transactionalBalance": 125430.50,
          "transactionTimestamp": "2025-06-15T14:20:10+05:30",
          "valueDate": "2025-06-15T00:00:00+05:30",
          "narration": "UPI/P2M/416712345678/MERCHANT",
          "reference": "416712345678"
        }
      ]
    }
  }
}
```

---

## 3. `EQUITIES` FI Schema (`equities.xsd`, Active v1.0.0)

- **Target Namespace**: `http://api.rebit.org.in/FISchema/equities`
- **Normative Active XSD**: `https://specifications.rebit.org.in/api_schema/account_aggregator/FISchema/equities.xsd`

> **Schema Drift Note**: `Sahamati/account-aggregator-standards/schemas/equity_shares/equities.xsd` holds an older pre-v1.0.0/draft variant (using `maskedAccNumber`, `Summary.investmentValue`, `Holdings.Holding.rate`/`investmentDateTime`/`description`, and `Transaction.tradeId`/`symbol`/`instrumentType`/`optionType`/`strikePrice`/`totalCharge`/`tradeValue`/`shareHolderEquityType`/`otherCharges`). The active ReBIT specification published on `specifications.rebit.org.in` is documented below.

### 3.1 `Account` Root Attributes

| Attribute | XSD Type | Required | Value / Format / Notes |
| :--- | :--- | :--- | :--- |
| `type` | `xs:string` | Yes | Fixed `"equities"` |
| `maskedDematId` | `xs:string` | Yes | Masked Demat ID *(note: `maskedDematId`, not `maskedAccNumber`)* |
| `version` | `aa:version` | Yes | Enum `"1.0.0"` |
| `linkedAccRef` | `xs:string` | Yes | FIP-assigned link reference number |

### 3.2 `Profile.Holders` & `Profile.Holders.Holder[]`

**`Holders`** (1..1 inside `Profile`): has **no `type` attribute** in `equities.xsd`.

**`Holder[]`** (1..unbounded inside `Holders`):

| Attribute | XSD Type | Required | Format / Enum / Notes |
| :--- | :--- | :--- | :--- |
| `name` | `xs:string` | Yes | Primary holder name |
| `mobile` | `xs:integer` | Yes | Primary mobile number |
| `dematId` | `xs:string` | Yes | Demat ID assigned to investor |
| `address` | `xs:string` | Yes | Primary holder address |
| `pan` | `aa:HolderPan` | Yes | Regex `[a-zA-Z]{5}[0-9]{4}[a-zA-Z]` |
| `dob` | `xs:date` | No | ISO date `YYYY-MM-DD` |
| `nominee` | `aa:HoldingNominee` | No | `"REGISTERED"` \| `"NOT-REGISTERED"` |
| `landline` | `xs:string` | No | Landline number |
| `email` | `aa:HolderEmail` | No | Regex `[^@]+@[^\.]+\..+` |
| `kycCompliance` | `xs:boolean` | No | `true` \| `false` |

### 3.3 `Summary`, `Investment.Holdings`, and `Holding[]`

**`Summary`**:
- `currentValue` (required `xs:float`): Current value of investment as on date.
- `Investment` (required `1..1`) -> `Holdings` (required `1..1`):
  - `type` (required `aa:HoldingMode`: `"DEMAT"` | `"PHYSICAL"`)
  - `Holding[]` (`1..unbounded`):

| Attribute | XSD Type | Required | Format / Notes |
| :--- | :--- | :--- | :--- |
| `issuerName` | `xs:string` | Yes | Equity issuer name |
| `isin` | `xs:string` | Yes | Security ISIN (e.g. `"INE002A01018"`) |
| `isinDescription` | `xs:string` | Yes | Long name of ISIN |
| `units` | `xs:positiveInteger` | Yes | Integer quantity held (`>= 1`) |
| `lastTradedPrice` | `xs:string` | Yes | Last traded / closing price as decimal string |

### 3.4 `Transactions` & `Transactions.Transaction[]`

**`Transactions`**:
- `startDate` (required `xs:date`, `YYYY-MM-DD`)
- `endDate` (required `xs:date`, `YYYY-MM-DD`)
- `Transaction[]` (`minOccurs="1"`, `maxOccurs="unbounded"`)

**`Transaction[]`**:

| Attribute | XSD Type | Required | Format / Enum / Notes |
| :--- | :--- | :--- | :--- |
| `txnId` | `xs:string` | Yes | Transaction ID |
| `orderId` | `xs:string` | No | Exchange/broker order ID |
| `companyName` | `xs:string` | Yes | Company name |
| `transactionDateTime` | `xs:dateTime` | Yes | ISO 8601 timestamp |
| `exchange` | `aa:TransactionsSymbol` | No | `"BSE"` \| `"NSE"` \| `" OTHERS"` *(note leading space in XSD enum)* |
| `isin` | `xs:string` | Yes | Security ISIN |
| `isinDescription` | `xs:string` | Yes | Long name of ISIN |
| `equityCategory` | `aa:EquityCategory` | Yes | Fixed `"EQUITY"` |
| `narration` | `xs:string` | No | Description / remark |
| `rate` | `xs:string` | No | Execution price per unit |
| `type` | `aa:TransactionsType` | Yes | `"BUY"` \| `"SELL"` \| `"BONUS"` \| `"SPLIT"` \| `"DIVIDEND"` \| `"RIGHTS"` \| `" OTHERS"` *(note leading space in XSD enum)* |
| `units` | `xs:positiveInteger` | Yes | Integer quantity (`>= 1`) |

> Note for `internal/aa`: Trim whitespace when matching `exchange` and `type` enums because the literal `equities.xsd` contains `" OTHERS"` with a leading space.

### 3.5 Representative `EQUITIES` v1.0.0 JSON Snippet

```json
{
  "Account": {
    "type": "equities",
    "maskedDematId": "XXXXXXXXXXXX0987",
    "version": "1.0.0",
    "linkedAccRef": "DEMAT-REF-001",
    "Profile": {
      "Holders": {
        "Holder": [
          {
            "name": "Asha Kulkarni",
            "dob": "1990-05-14",
            "mobile": "9876543210",
            "nominee": "REGISTERED",
            "dematId": "XXXXXXXXXXXX0987",
            "address": "12 MG Road, Bengaluru",
            "email": "asha@example.com",
            "pan": "ABCDE1234F",
            "kycCompliance": true
          }
        ]
      }
    },
    "Summary": {
      "currentValue": 84500.00,
      "Investment": {
        "Holdings": {
          "type": "DEMAT",
          "Holding": [
            {
              "issuerName": "RELIANCE INDUSTRIES LTD",
              "isin": "INE002A01018",
              "isinDescription": "RELIANCE INDUSTRIES LTD EQ",
              "units": 30,
              "lastTradedPrice": "2816.65"
            }
          ]
        }
      }
    },
    "Transactions": {
      "startDate": "2025-01-01",
      "endDate": "2025-07-01",
      "Transaction": [
        {
          "txnId": "EQ-TXN-1001",
          "orderId": "ORD-9001",
          "companyName": "RELIANCE INDUSTRIES LTD",
          "transactionDateTime": "2025-02-04T10:00:00+05:30",
          "exchange": "NSE",
          "isin": "INE002A01018",
          "isinDescription": "RELIANCE INDUSTRIES LTD EQ",
          "equityCategory": "EQUITY",
          "narration": "BUY",
          "rate": "2750.00",
          "type": "BUY",
          "units": 30
        }
      ]
    }
  }
}
```

---

## 4. Identifier Optionality, Masking, and Cross-FIP Behaviour

1. **Schema-optional attributes**:
   - In `DEPOSIT` v2.0.0 (`deposit_v2.0.0.xsd`), `pan`, `email`, `address`, `landline`, and `nominee` on `Holder` are `use="optional"` and may be absent, `null`, or `""` depending on FIP records.
   - In `EQUITIES` v1.0.0 (`equities.xsd`), `dob`, `email`, `nominee`, `landline`, and `kycCompliance` on `Holder` are `use="optional"`. On `Transaction`, `orderId`, `exchange`, `narration`, and `rate` are `use="optional"` (commonly omitted on corporate actions like `BONUS`/`SPLIT`/`DIVIDEND`).
2. **Masking in FI & transport payloads**:
   - `Account.maskedAccNumber` (`DEPOSIT`), `Account.maskedDematId` (`EQUITIES`), and `FI[].data[].maskedAccNumber` are masked by FIPs (e.g. `XXXXXXXX4020`, `XXXXXXXXXXXX0987`).
   - `Holder.dematId` in `EQUITIES` is commonly masked in the same format as `maskedDematId`.
   - In practice across FIPs, even XSD-`required` KYC identifiers (`pan`, `mobile`, `dob`, `email`) may occasionally be masked (e.g. `XXXXX1234X`) or blank when legacy core-banking/depository records lack a verified value or when FIP data-minimization policies apply; `internal/aa` should treat missing/masked `Holder` identifiers gracefully rather than failing account ingestion.
3. **Consent-scoped blocks**:
   - `Profile`, `Summary`, and `Transactions` each have `minOccurs="0"` on `Account` in both schemas; a consent granted only for `SUMMARY` + `TRANSACTIONS` will omit `Profile` entirely.

---

## 5. Primary Sources

- ReBIT Deposit v2.0.0 XSD: https://specifications.rebit.org.in/api_schema/account_aggregator/FISchema/deposit_v2.0.0.xsd
- ReBIT Deposit v2.0.0 Release Notes: https://specifications.rebit.org.in/api_schema/account_aggregator/ReleaseNotes/Deposit_v2.0.0_Release_Note.txt
- ReBIT Deposit v2.0.0 Adoption Circular: https://specifications.rebit.org.in/templates/NBFC-AA_Guidelines_Adoption_Deposit_RD_TD_FI_Type_Schema_Version_2.0.0.pdf
- ReBIT Deposit v1.x XSD: https://specifications.rebit.org.in/api_schema/account_aggregator/FISchema/deposit.xsd
- ReBIT Equities v1.0.0 XSD (Active): https://specifications.rebit.org.in/api_schema/account_aggregator/FISchema/equities.xsd
- Sahamati FI Schema v2.0.0 Adoption Guide & FAQs: https://github.com/Sahamati/AA-Ecosystem-FISchema-v2-Adoption
- Sahamati Account Aggregator Standards (v1.x XML/XSD archive & OpenAPI specs): https://github.com/Sahamati/account-aggregator-standards
- Sahamati Official AA v2.x OpenAPI Specs (`AA-v2-latest.yaml`, `FIP-v2-latest.yaml`): https://github.com/Sahamati/documentation
- Sahamati Certification Framework (`FIP_3009_POST_FI_Fetch.feature`, `AA_2025_POST_FI_Fetch.feature`): https://github.com/Sahamati/certification-framework
