# MFapi.in Endpoints, Response Schema, and Verified AMFI Scheme Codes

> **Ticket**: #5 (`wayfinder:research`)
> **Verified on**: 2026-11-27 against live `api.mfapi.in` responses, the official `mfapi.in` OpenAPI 3.0.3 specification, and AMFI `NAVAll.txt`.
> **Purpose**: Anchor ReconcileX's NAV client, synthetic CAS fixtures, and offline `fixtures/nav/` snapshots to verified primary-source schemas and active Indian Direct Growth mutual fund schemes.

---

## 1. Primary Sources

- **MFapi.in Homepage** (update cadence, auth policy): <https://www.mfapi.in/>
- **MFapi.in API Documentation** (rate limiting & usage notes): <https://www.mfapi.in/docs/>
- **MFapi.in OpenAPI 3.0.3 Specification** (`v1.0.0`): <https://www.mfapi.in/docs/openapi.json>
- **Live API Base URL**: <https://api.mfapi.in>
- **AMFI Daily NAV Feed** (cross-verification of scheme codes, names, ISINs, and NAVs): <https://www.amfiindia.com/spages/NAVAll.txt>

---

## 2. Endpoints & Query Parameters

All endpoints use `GET`, require **no authentication or API keys**, and return `Content-Type: application/json` (supporting `Accept-Encoding: gzip`).

| Endpoint | Query Parameters | Description |
| :--- | :--- | :--- |
| `GET /mf/{scheme_code}` | `startDate` (`YYYY-MM-DD`, optional), `endDate` (`YYYY-MM-DD`, optional) | Returns scheme metadata (`meta`) and historical NAV array (`data[]`) in reverse chronological order (newest first). Passing `DD-MM-YYYY` to `startDate`/`endDate` is silently ignored and returns full history; ISO 8601 `YYYY-MM-DD` filters accurately. |
| `GET /mf/{scheme_code}/latest` | None | Same envelope (`meta`, `data`, `status`) as `/mf/{scheme_code}`, with `data` containing only the single most recent NAV entry (`data.length == 1` when found). |
| `GET /mf/search` | `q` (required, string) | Full-text search returning `[{"schemeCode": int, "schemeName": string}, ...]`. Missing/empty `q` returns HTTP `400` with `[]`; no matches returns HTTP `200` with `[]`. |
| `GET /mf` | `limit` (`1..1000`, optional), `offset` (`>= 0`, optional) | Lists available schemes with `schemeCode`, `schemeName`, `isinGrowth`, `isinDivReinvestment` (camelCase). Without `limit`, returns the full catalog (~38,083 schemes observed on 2026-11-27). |
| `GET /mf/latest` | None | Bulk cached array of latest NAV records across all schemes (~38,083 items) using camelCase keys (`schemeCode`, `schemeName`, `fundHouse`, `schemeType`, `schemeCategory`, `isinGrowth`, `isinDivReinvestment`, `nav`, `date`). |

---

## 3. Exact JSON Schema: `/mf/{scheme_code}` and `/mf/{scheme_code}/latest`

Both `GET /mf/{scheme_code}` and `GET /mf/{scheme_code}/latest` conform to the `NAVHistoryResponse` schema defined in `https://www.mfapi.in/docs/openapi.json`:

```json
{
  "meta": {
    "fund_house": "string",
    "scheme_type": "string",
    "scheme_category": "string",
    "scheme_code": 122639,
    "scheme_name": "string",
    "isin_growth": "string | null",
    "isin_div_reinvestment": "string | null"
  },
  "data": [
    {
      "date": "DD-MM-YYYY",
      "nav": "0.00000"
    }
  ],
  "status": "SUCCESS"
}
```

### Field Reference

| Path | Type | Required (OpenAPI) | Format / Notes |
| :--- | :--- | :--- | :--- |
| `meta` | `object` (`SchemeMeta`) | Yes | Scheme metadata object (snake_case keys). |
| `meta.fund_house` | `string` | No | AMC name (e.g., `"PPFAS Mutual Fund"`, `"HDFC Mutual Fund"`). |
| `meta.scheme_type` | `string` | No | Structure type (e.g., `"Open Ended Schemes"`). |
| `meta.scheme_category` | `string` | No | AMFI category string (e.g., `"Equity Scheme - Flexi Cap Fund"`, `"Equity Schemes - ELSS- Tax Saver Fund"` — note minor phrasing variations across AMCs). |
| `meta.scheme_code` | `integer` | Yes | Numeric AMFI scheme code (e.g., `122639`). Returns `0` when an unknown integer is queried. |
| `meta.scheme_name` | `string` | Yes | Official AMFI scheme name. |
| `meta.isin_growth` | `string \| null` | No | 12-character ISIN for Growth / IDCW-Payout option (e.g., `"INF879O01027"`), or `null` if unavailable. |
| `meta.isin_div_reinvestment` | `string \| null` | No | 12-character ISIN for IDCW-Reinvestment option, or `null` for Growth plans. |
| `data` | `array<NAVData>` | Yes | Ordered newest-to-oldest (`data[0]` is the latest NAV). Single-element array on `/latest`. |
| `data[].date` | `string` | Yes | Valuation date formatted strictly as **`DD-MM-YYYY`** (zero-padded, e.g., `"26-11-2026"`). Note: AMFI's raw `NAVAll.txt` uses `DD-Mon-YYYY`, whereas `api.mfapi.in` normalizes it to `DD-MM-YYYY`. |
| `data[].nav` | `string` | Yes | Decimal NAV serialized as a **string with 5 decimal places** (e.g., `"96.50130"`, `"1296.57800"`). Always parse with a decimal/fixed-point type rather than `float64`. |
| `status` | `string` | Yes | OpenAPI enum: `"SUCCESS"` or `"ERROR"`. |

---

## 4. Sample JSON Responses (Observed Live on 2026-11-27)

### `GET https://api.mfapi.in/mf/122639/latest`

```json
{
  "meta": {
    "fund_house": "PPFAS Mutual Fund",
    "scheme_type": "Open Ended Schemes",
    "scheme_category": "Equity Scheme - Flexi Cap Fund",
    "scheme_code": 122639,
    "scheme_name": "Parag Parikh Flexi Cap Fund - Direct Plan - Growth",
    "isin_growth": "INF879O01027",
    "isin_div_reinvestment": null
  },
  "data": [
    {
      "date": "25-11-2026",
      "nav": "95.76010"
    }
  ],
  "status": "SUCCESS"
}
```

### `GET https://api.mfapi.in/mf/122639?startDate=2026-11-24&endDate=2026-11-26` (Filtered History)

```json
{
  "meta": {
    "fund_house": "PPFAS Mutual Fund",
    "scheme_type": "Open Ended Schemes",
    "scheme_category": "Equity Scheme - Flexi Cap Fund",
    "scheme_code": 122639,
    "scheme_name": "Parag Parikh Flexi Cap Fund - Direct Plan - Growth",
    "isin_growth": "INF879O01027",
    "isin_div_reinvestment": null
  },
  "data": [
    {
      "date": "26-11-2026",
      "nav": "96.50130"
    },
    {
      "date": "25-11-2026",
      "nav": "95.76010"
    }
  ],
  "status": "SUCCESS"
}
```

---

## 5. Error & Not-Found Behavior (Spec vs. Live Production)

Testing live `api.mfapi.in` on `2026-11-27` revealed a **critical divergence** between `openapi.json` and production behavior:

| Condition | OpenAPI Spec (`openapi.json`) | Live Production Response (Observed 2026-11-27) |
| :--- | :--- | :--- |
| **Unknown integer `scheme_code`** (e.g., `/mf/999999999`, `/mf/999999999/latest`, `/mf/0`, `/mf/-1`) | HTTP `404` (`Scheme not found`) | **HTTP `200 OK`** with empty sentinel payload: `{"meta":{"fund_house":"","scheme_type":"","scheme_category":"","scheme_code":0,"scheme_name":"","isin_growth":null,"isin_div_reinvestment":null},"data":[],"status":"SUCCESS"}` |
| **Non-numeric `scheme_code`** (e.g., `/mf/abc`, `/mf/nonexistent`) | Not explicitly listed | **HTTP `400 Bad Request`** with `{"error":"invalid scheme_code"}` |
| **Missing/empty search query** (`/mf/search?q=`) | HTTP `400` (`Bad request - missing search query`) | **HTTP `400 Bad Request`** with `[]` |
| **No search matches** (`/mf/search?q=zzznotafundzzz`) | HTTP `200` with `[]` | **HTTP `200 OK`** with `[]` |

**Implementation Implication for ReconcileX**: Do not rely solely on HTTP status codes or `status == "ERROR"` to detect missing schemes. Treat `meta.scheme_code == 0` or `len(data) == 0` (when querying without a narrow weekend/holiday date filter) as a **scheme-not-found** condition, and also handle HTTP `400` / `404` / `429` / `5xx`.

---

## 6. Update Cadence, Caching, and Rate-Limiting Considerations

1. **Upstream Update Schedule**: According to <https://www.mfapi.in/>, data is refreshed **6 times daily** (`10:05 AM`, `2:05 PM`, `6:05 PM`, `9:05 PM`, `3:09 AM`, `5:05 AM` IST). AMFI publishes end-of-day equity/debt NAVs late evening IST, so a query earlier in the day reflects the prior business day's NAV.
2. **Edge Caching (`nginx` + `Cache-Control`)**:
   - Live responses include `X-Cache-Status: HIT` or `MISS` and `Cache-Control: public, max-age=21600` (6 hours, observed on `/mf`).
   - Because `/mf/{code}/latest` and `/mf/{code}` are cached independently, `/latest` can occasionally lag by one business day if its cache entry has not expired yet: on `2026-11-27`, `GET /mf/122639/latest` returned `25-11-2026` (`95.76010`) while `GET /mf/122639?startDate=2026-11-01&endDate=2026-11-26` returned `26-11-2026` (`96.50130`, matching AMFI `NAVAll.txt`).
   - ReconcileX should support fetching via `/mf/{scheme_code}?startDate=...&endDate=...` when exact date-aligned valuation is needed, and cache NAV lookups locally (per `(scheme_code, date)` or TTL) to avoid redundant network calls.
3. **Rate Limiting**:
   - The homepage markets "no rate limiting" for normal use, whereas `https://www.mfapi.in/docs/` explicitly states: *"The API implements rate limiting to ensure fair usage. Please cache responses when appropriate to minimize API calls."*
   - No `X-RateLimit-*` headers were present on `200`/`400` responses during single-request probes, so exact burst thresholds are undocumented. ReconcileX should implement polite client-side rate limiting / concurrency caps, exponential backoff on HTTP `429`/`5xx`, and **deterministic offline snapshots** under `fixtures/nav/` for tests and CI.
4. **Payload Size**: Full `/mf/{scheme_code}` history without `startDate` can exceed 125 KB uncompressed (~3,200–3,320 daily entries for 2013-era Direct plans). Always send `Accept-Encoding: gzip` and pass `startDate`/`endDate` (`YYYY-MM-DD`) when full history is unnecessary.

---

## 7. Verified Active Indian Direct Growth Schemes (8 Anchors)

All 8 schemes below are active **Direct Plan — Growth** schemes verified live on **2026-11-27** against both `https://api.mfapi.in/mf/{scheme_code}` and AMFI's `https://www.amfiindia.com/spages/NAVAll.txt`. They span all five target asset categories for ReconcileX's synthetic CAS fixtures and offline `fixtures/nav/` snapshots.

| Asset Class | `scheme_code` | Exact `scheme_name` (`mfapi.in`) | `isin_growth` | `fund_house` | `scheme_category` | Latest Verified NAV (`date`) |
| :--- | :---: | :--- | :--- | :--- | :--- | :--- |
| **Large Cap** | `119018` | `HDFC Large Cap Fund - Direct Plan - Growth Option` | `INF179K01YV8` | `HDFC Mutual Fund` | `Equity Scheme - Large Cap Fund` | `1296.57800` (`26-11-2026`) |
| **Large Cap** | `120586` | `ICICI Prudential Large Cap Fund (erstwhile Bluechip Fund) - Direct Plan - Growth` | `INF109K016L0` | `ICICI Prudential Mutual Fund` | `Equity Scheme - Large Cap Fund` | `126.62000` (`26-11-2026`) |
| **Flexi Cap** | `122639` | `Parag Parikh Flexi Cap Fund - Direct Plan - Growth` | `INF879O01027` | `PPFAS Mutual Fund` | `Equity Scheme - Flexi Cap Fund` | `96.50130` (`26-11-2026`) |
| **Mid Cap** | `118989` | `HDFC Mid Cap Fund - Direct Plan - Growth Option` | `INF179K01XQ0` | `HDFC Mutual Fund` | `Equity Scheme - Mid Cap Fund` | `231.70600` (`26-11-2026`) |
| **ELSS** | `135781` | `Mirae Asset ELSS Tax Saver Fund - Direct Plan - Growth` | `INF769K01DM9` | `Mirae Asset Mutual Fund` | `Equity Schemes - ELSS- Tax Saver Fund` | `59.93400` (`26-11-2026`) |
| **ELSS** | `120503` | `Axis ELSS- Tax Saver Fund - Direct Plan - Growth Option` | `INF846K01EW2` | `Axis Mutual Fund` | `Equity Schemes - ELSS- Tax Saver Fund` | `117.85560` (`26-11-2026`) |
| **Liquid Debt** | `119800` | `SBI LIQUID FUND - Direct Plan - Growth` | `INF200K01UT4` | `SBI Mutual Fund` | `Debt Scheme - Liquid Fund` | `4495.93110` (`26-11-2026`) |
| **Overnight Debt** | `119110` | `HDFC Overnight Fund - Direct Plan - Growth Option` | `INF179KB1HT1` | `HDFC Mutual Fund` | `Debt Scheme - Overnight Fund` | `4129.67430` (`26-11-2026`) |

> **Fixture Notes**:
> - For every Direct Plan Growth scheme above, `isin_div_reinvestment` is `null`.
> - Notice capitalization and punctuation quirks in AMFI/MFapi names (e.g., `SBI LIQUID FUND`, `Axis ELSS- Tax Saver Fund`, `(erstwhile Bluechip Fund)`). Matching by **`isin_growth`** first and falling back to normalized scheme name or `scheme_code` ensures CAS-to-MFapi reconciliation remains resilient across AMC rebrandings (such as *HDFC Top 100* → *HDFC Large Cap* and *ICICI Prudential Bluechip* → *ICICI Prudential Large Cap*).
