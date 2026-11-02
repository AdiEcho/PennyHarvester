# Predict.fun — settlement / resolution / redemption (primary-source research)

Scope: what a Go backend needs to (a) detect that a Predict.fun market resolved and which
outcome won, (b) redeem the winning position on-chain, (c) sign for a smart wallet.
Written 2026-11-02. Every claim below is tagged:

- **[V]** verified from live API JSON / on-chain call / SDK source that I read.
- **[I]** inferred (stated as such, with the reasoning).
- **[?]** unknown — do not guess; listed again in §8.

Everything "live" was fetched keyless from the **testnet host `https://api-testnet.predict.fun`**.
The **mainnet host `https://api.predict.fun` returns `401 unauthorized` for every `GET`
(including `GET /v1/markets`) without an `x-api-key` header** [V]. I had no mainnet key, so no
mainnet JSON is quoted here. The **OpenAPI schema is identical on both hosts**: both
`https://api.predict.fun/docs` and `https://api-testnet.predict.fun/docs` serve the same inline
`let spec = { "title": "Predict REST API", "version": "1.0.0" }` document [V]. That spec is the
authority for field/enum questions below; the Go repo already sends `x-api-key`.

---

## 1. Sources

| Source | What it is | Notes |
|---|---|---|
| `https://api-testnet.predict.fun/docs` | **Full OpenAPI 3.0 spec, 110 paths, 364 schemas, embedded inline in the HTML** (`let spec = {...}`) | Primary. Not a redoc page — you can `grep` the enums out of it. |
| `https://api.predict.fun/docs` | same spec, same version | mainnet |
| `https://api-testnet.predict.fun/v1/markets?...` | live market JSON | keyless |
| `https://github.com/PredictDotFun/sdk` (`src/Constants.ts`, `src/Types.ts`, `src/OrderBuilder.ts`, `src/internal/Utils.ts`, `src/abis/*.json`, `README.md`) | official TS SDK | primary for calldata |
| `https://github.com/PredictDotFun/sdk-python` (`src/predict_sdk/order_builder.py`, `constants.py`, `README.md`) | official Python SDK | mirrors the TS one; used to cross-check |
| `https://developers.predict.fun` | now a **"Predict \| Developer Dashboard"** (API-key UI), *not* docs [V] | no redemption prose |
| `https://docs.predict.fun` | TLS handshake fails (curl exit 35) [V] | matches the report that the docs site is offline/compromised — do not trust mirrors |
| BSC RPC `eth_call` on chain 56 / 97 | `payoutDenominator`, `payoutNumerators`, `NegRiskAdapter.getDetermined` | see §3.6 |

Line numbers below are against `main` as fetched on 2026-11-02.

---

## 2. Market resolution data (API)

### 2.1 The two independent status fields

`GET /v1/markets`, `GET /v1/markets/{id}`, `GET /v1/markets/oracle/{oracle_question_id}` all return
the `Market` schema with **two different status fields** [V, OpenAPI `components/schemas/Market`]:

```jsonc
"tradingStatus": "OPEN" | "MATCHING_NOT_ENABLED" | "CANCEL_ONLY" | "CLOSED",
"status":        "REGISTERED" | "PRICE_PROPOSED" | "PRICE_DISPUTED" | "PAUSED" | "UNPAUSED" | "RESOLVED" | "REMOVED"
```

- `MarketTradingStatus` — "Trading status derived from market visibility and deletion state."
  It is the **order-entry gate**, not the lifecycle. Only `OPEN` allows new orders.
- `MarketStatus` — "Market lifecycle state, from registration through resolution. `Removed` is a
  synthetic status reported for markets that have been withdrawn, meaning it is not part of the
  normal lifecycle progression." Note `PRICE_DISPUTED` and `UNPAUSED` exist — this is a
  Polymarket-style propose/dispute oracle, so **`PRICE_PROPOSED` is not final**.

Related enums in the same schema set [V]:

```
MarketStatusFilter   : "OPEN" | "RESOLVED"        <- the ONLY legal values of ?status=  (see 2.4)
OutcomeStatus        : "WON" | "LOST"             <- outcome-level; null while unresolved
OutcomeSide          : "Yes" | "No"
ResolutionProvider   : "PREDICT_DOT_FUN" | "THREE_PO" | "CHAINLINK" | "ATLAS"
Protocol (on category): "LEGACY" | "PREDICT"      <- decides which exchange set a market trades on
MarketVariant        : DEFAULT, SPORTS_*, CRYPTO_UP_DOWN, TWEET_COUNT, UP_DOWN, ESPORTS_*, ...
Resolution (timeseries query param only): "1m"|"5m"|"1h"|"1d"|"1w"|"1M"
                                              <- unrelated to settlement; do not confuse with Market.resolution (§2.2)
```

### 2.2 `resolution` IS the winner — it is an `Outcome` object, not an enum

This is the single most important finding, and the current Go code gets it wrong.
`Market.resolution` is typed `$ref: Outcome` (it is *not* a string, and there is **no**
`winningIndexSet` / `resolvedAt` / `settlementPrice` / payout field anywhere in the API) [V].

Real response, `GET https://api-testnet.predict.fun/v1/markets/799` [V]:

```json
{
  "id": 799,
  "status": "RESOLVED",
  "tradingStatus": "CLOSED",
  "isVisible": true,
  "isNegRisk": true,
  "conditionId": "0xcac49439cff87d4bbad572e3626da48d7b4fc77d2903da3538a71696793470ef",
  "oracleQuestionId": "0xe8d4f5a6359107b80c33af4a2210831b186c8615dd6497c608993a0dfd221018",
  "resolverAddress": "0x52DA245ac170155391e7607c67b77D549005002d",
  "resolution": {
    "indexSet": 1,
    "name": "Yes",
    "onChainId": "51061391110964462364816549846698195046327725186109140232213939168405908487696",
    "positionId": "51061391110964462364816549846698195046327725186109140232213939168405908487696",
    "status": "WON",
    "variantData": null
  },
  "outcomes": [
    { "indexSet": 1, "name": "Yes", "status": "WON",  "bestBid": null, "bestAsk": null, ... },
    { "indexSet": 2, "name": "No",  "status": "LOST", "bestBid": null, "bestAsk": null, ... }
  ]
}
```

Rules that follow, all [V] from ~60 resolved markets on the testnet catalogue:

1. **Winner = `resolution.indexSet`** (1 = first outcome / usually Yes|Up, 2 = second / No|Down).
   `resolution` is `null` while the market is unresolved (every non-resolved market in the 20-market
   sample I checked had `resolution: null`).
2. **Per-outcome `status`** is `"WON"` / `"LOST"` and is `null` before resolution. `outcomes[].status`
   and `resolution.status` agree; `resolution` is a *copy of the winning outcome object*.
3. **Resolved markets have `tradingStatus: "CLOSED"` and `status: "RESOLVED"`**, `isVisible` stays
   `true`, and `bestBid`/`bestAsk` become `null` on both outcomes.
4. `indexSet` on the wire is an **int**; `onChainId`/`positionId` are **decimal strings** (77 digits —
   must not go through float64; the repo's `Number` type is right).
5. `onChainId` is marked **deprecated** in the spec: *"Deprecated, removed with v1. Use `positionId`,
   the same value under the name the Predict exchange uses."* → read `positionId`, fall back to
   `onChainId` for old payloads [V, `components/schemas/Outcome`].
6. `conditionId` **is** exposed on the market object (question 4 answered: yes) — it is the value to
   pass to `redeemPositions`. It equals
   `keccak256(abi.encodePacked(resolverAddress, oracleQuestionId, uint256(2)))` — I recomputed this
   for market 29076 and got an exact match [V]. (Useful as a sanity check, not as a replacement.)

**Split / 50-50 resolutions exist and break "one winner" logic.** Market 1388 ("50-50 event") has
`resolution: {"name":"Yes","indexSet":1,"status":"WON"}` **and** `outcomes[1].status == "WON"` — two
outcomes marked WON — and on-chain `payoutDenominator = 2`, `payoutNumerators = [1, 1]` (i.e. 0.50
per share for *both* sides) [V]. Its status log shows how that is produced:
`PRICE_PROPOSED` with `proposedOutcomeId: 3, proposedOutcomeLabel: "Draw"` (§2.3).
So: **`resolution.indexSet` alone under-describes the market; also read `outcomes[].status` and, for
money, read on-chain numerators (§3.6).**

### 2.3 Status-transition log — the "when did it resolve" feed

`GET /v1/markets/status-logs?ids=29076,1388` (up to 100 ids, `ids` required, oldest first, markets
with no transitions omitted) [V]. Live response fragment:

```json
{"data": [{"marketId": 29076, "logs": [
  {"status":"REGISTERED",     "timestamp":"2026-02-11T14:30:24.000Z","transactionHash":"0xe3a6...","resolutionProvider":"PREDICT_DOT_FUN","proposedOutcomeId":null,"proposedOutcomeLabel":null,"logIndex":null},
  {"status":"PRICE_PROPOSED", "timestamp":"2026-02-11T14:46:25.000Z","transactionHash":"0x7c50...","resolutionProvider":"PREDICT_DOT_FUN","proposedOutcomeId":2,"proposedOutcomeLabel":"Down","logIndex":null},
  {"status":"RESOLVED",       "timestamp":"2026-02-12T11:05:34.000Z","transactionHash":"0x8836...","resolutionProvider":"PREDICT_DOT_FUN","proposedOutcomeId":null,"proposedOutcomeLabel":null,"logIndex":null}
]}]}
```

`MarketStatusLog` fields: `status`, `timestamp`, `transactionHash`, `logIndex`, `proposedOutcomeId`,
`proposedOutcomeLabel`, `resolutionProvider`. Per the spec, `proposedOutcomeId` is *"The outcome
proposed for resolution: 1 and 2 are the outcomes with that `indexSet`, 3 is a draw. Only set on
`PRICE_PROPOSED` entries."* [V]

Note the ~20 h gap between `PRICE_PROPOSED` and `RESOLVED` above: a bot that treats
`PRICE_PROPOSED` as settled would be early, and a dispute (`PRICE_DISPUTED`) can still flip it.

Also available, both non-authoritative for payout: `GET /v1/markets/{id}/oracle-updates`
(append-only clarification text while live) and `GET /v1/markets/{id}/bulletin-board-updates` [V].

### 2.4 Filtering resolved markets: `?status=` takes exactly two values

`GET /v1/markets?status=RESOLVED` works; `status=CLOSED`, `SETTLED`, `PAUSED`, `REGISTERED`,
`CANCEL_ONLY`, `MATCHING_NOT_ENABLED` all return **HTTP 400**
`failed to parse parameter \`status\`: failed to parse "MarketStatusFilter": Expect a valid enumeration value` [V].
`MarketStatusFilter` = `OPEN | RESOLVED` only [V]; `RESOLVED` returns markets with
`status: RESOLVED`, `tradingStatus: CLOSED`. Paging is `?first=` (**`first=100` works — `first` is the real
param name**) + `?after=<cursor>` (base64 cursor) [V]. `?limit=` is **silently ignored** and returns
20 rows, which reads like a short page and will make you believe there are only 20 resolved markets [V].
The *existing* repo code passes `Status: "OPEN"` for scanning; for settlement use `RESOLVED` and/or
poll `GET /v1/markets/{id}` for markets you hold.

### 2.5 `GET /v1/positions` — what it gives you

Requires **both** `x-api-key` and `Authorization: Bearer <JWT>` (`security: [{ApiKeyAuth},{BearerAuth}]`)
[V, OpenAPI]. Query params: `first`, `after`, `marketId`, `marketIds` (≤100, comma-separated),
`categoryId`, **`isResolved`** (bool), **`includeLost`** (bool), `sort` [V].

`PositionData` [V]:

```jsonc
{ "id": "…", "market": {…full Market, incl. status/resolution/conditionId…},
  "outcome": {…Outcome, incl. indexSet, positionId, status…},
  "amount": "bigint string", "valueUsd": "string",
  "averageBuyPriceUsd": "string", "pnlUsd": "string" }
```

There is **no `redeemable` flag, no `payout`, no `settled` boolean, no transaction hash** [V].
Explicit spec note on `includeLost`: *"Also list positions in outcomes that settled as losing. Off by
default, since those need no action from the holder."* → the intended query for a redemption worker is
`GET /v1/positions?isResolved=true` (winning positions only by default) and then act when
`market.status == "RESOLVED"` [I, from that sentence + the field set].

Redemption history, if you need to confirm a redeem landed: `ActivityEventName` includes
`"REDEEM"` (also `CONVERT`, `MERGE`, `SPLIT`, `REFUND`) on `GET /v1/account/activity` [V].

---

## 3. Redemption / settlement mechanics

### 3.1 The two paths (and the exact method signatures)

TS SDK `src/OrderBuilder.ts` [V]:

```ts
async redeemPositions(options: RedeemPositionsOptions): Promise<TransactionResult>   // L681
async mergePositions(options: MergePositionsOptions): Promise<TransactionResult>     // L742
async splitPositions(options: SplitPositionsOptions): Promise<TransactionResult>     // L803
async convertPositions(options: ConvertPositionsOptions): Promise<TransactionResult> // L866
```

`src/Types.ts:399` [V]:

```ts
export interface RedeemPositionsOptions {
  conditionId: string;
  indexSet: 1 | 2;
  isNegRisk: boolean;
  isYieldBearing: boolean;
  /** Required when isNegRisk is true */
  amount?: bigint;
}
export interface ConvertPositionsOptions {   // L422
  negRiskOnChainId: string;  // category's NegRisk market id, 32-byte hex — NOT the numeric market id
  indexSet: bigint;          // bitmask of NO positions, bit n = question at index n
  amount: bigint;
  isYieldBearing: boolean;
}
```

Python SDK `src/predict_sdk/order_builder.py` [V]:

```python
async def redeem_positions_async(self, condition_id: str, index_set: Literal[1,2],
                                 amount: int | None = None, *,
                                 is_neg_risk: bool, is_yield_bearing: bool) -> TransactionResult  # L1552
def     redeem_positions(...)        # L1645 sync wrapper
async def merge_positions_async(self, condition_id, amount, *, is_neg_risk, is_yield_bearing)       # L1667
async def split_positions_async(self, condition_id, amount, *, is_neg_risk, is_yield_bearing)       # L1774
async def convert_positions_async(self, neg_risk_on_chain_id, index_set, amount, *, is_yield_bearing)# L1881
```

There is **no `withdraw`** in either SDK — proceeds land as USDT in the signer/smart-wallet balance.

### 3.2 Plain (non-neg-risk) market → ConditionalTokens directly

`OrderBuilder.ts:708-724` [V]:

```ts
const identifier = isYieldBearing ? "YIELD_BEARING_CONDITIONAL_TOKENS" : "CONDITIONAL_TOKENS";
const amounts = [BigInt(indexSet)];
// EOA:
return this.handleTransaction(contract.redeemPositions,
  this.addresses.USDT, ZeroHash, conditionId, amounts);
```

i.e. **the canonical Gnosis CTF call, no helper/adapter** — confirms the question in the brief:

```
ConditionalTokens.redeemPositions(address collateralToken, bytes32 parentCollectionId,
                                 bytes32 conditionId, uint256[] indexSets)
selector 0x01b7037c   (keccak("redeemPositions(address,bytes32,bytes32,uint256[])")[0:4], computed)
```
ABI signature verified present in `src/abis/ConditionalTokens.json` [V].

Exact arguments for a binary Predict.fun market [V]:

| arg | value |
|---|---|
| `collateralToken` | `Addresses.USDT` (mainnet `0x55d398326f99059fF775485246999027B3197955`) |
| `parentCollectionId` | `bytes32(0)` — the SDK passes `ZeroHash` |
| `conditionId` | `market.conditionId` from the API |
| `indexSets` | **`[1]` for the Yes/Up/first outcome, `[2]` for No/Down/second** — the API's `outcomes[].indexSet` *is* the CTF index set, and the SDK passes `[indexSet]` verbatim |

So the brief's assumption (`1` = Yes, `2` = No) is **correct and confirmed**, but the authority is
`market.outcomes[].indexSet`, not the name — sports/crypto variants use `Up`/`Down`,
`Definitely`/`Maybe`, team names, etc. [V]

Pass `[1,2]` only if you want to burn both sides (a losing position redeems 0 — pointless, and the
API's `includeLost` default says so).

### 3.3 Neg-risk market → NegRiskAdapter, and `amount` is mandatory

`OrderBuilder.ts:688-707` [V]:

```ts
if (amount === undefined) throw new Error("amount is required for NegRisk markets");
const identifier = isYieldBearing ? "YIELD_BEARING_NEG_RISK_ADAPTER" : "NEG_RISK_ADAPTER";
const amounts = indexSet === 1 ? [amount, 0n] : [0n, amount];
return this.handleTransaction(contract.redeemPositions, conditionId, amounts);
```

```
NegRiskAdapter.redeemPositions(bytes32 conditionId, uint256[] amounts)   // amounts = [yesAmount, noAmount]
selector 0xdbeccb23
```
ABI verified in `src/abis/NegRiskAdapter.json` [V]. So **neg-risk redemption goes through the
adapter, not plain CTF** — answer to the brief's question. Note the adapter API takes *amounts*, so you
can redeem a partial balance (and the SDK's `amount` is the raw 18-decimal share count, same units as
`PositionData.amount`).

`convertPositions` is **not** a prerequisite for redemption. It exists for the "convert NO positions"
flow (used when you hold NO in a multi-outcome market and want YES elsewhere plus collateral):
`NegRiskAdapter.convertPositions(bytes32 negRiskId, uint256 indexSet, uint256 amount)` selector
`0xc64748c4`, needs the **category's** `negRiskOnChainId` (from `GET /v1/categories/{slug}` →
`CategoryWithStats.negRiskOnChainId`, e.g. `0xec98943c…f3900` for the `cz-tweet-count-december-2025`
category) [V]. Don't mix it up with settlement.

### 3.4 Approvals each path needs (this bites in production)

Python SDK, `order_builder.py:1246` [V] — the authoritative statement:

```python
if op == "REDEEM":
    # Neg risk claims redeem via the adapter; standard redemptions burn the user's tokens.
    return [erc1155(adapter_key)] if scope.is_neg_risk else []
```

- **Plain CTF redeem: no approval at all.** You burn your own ERC-1155, so `CTF_EXCHANGE`-style
  `setApprovalForAll` is irrelevant.
- **Neg-risk redeem: `setApprovalForAll(NEG_RISK_ADAPTER, true)` on the CTF is required** (the adapter
  pulls your shares). Same for `CONVERT` and for neg-risk `MERGE`
  ([V] same function + `Constants.ts` `APPROVAL_STEP_COPY["NEG_RISK_ADAPTER:ERC1155_APPROVAL"]` =
  *"Allows the multi-outcome adapter to manage your tokens."*).
- The repo already has approval plumbing (`backend/internal/predictfun/approvals.go`) and
  `addresses.go` has `NegRiskAdapter`; the missing piece is the adapter ERC-1155 approval (the file's
  header comment only mentions exchange approval + USDT allowance — verify before shipping).

`isMergePositionsGated()` / `isSplitPositionGated()` / `isConvertPositionsGated()` exist on
`ConditionalTokens` and `NegRiskAdapter` (`src/abis/*.json`) — if a gate is on, merge/split/convert
pauses while redeem keeps working [I: standard Gnosis gating semantics, worth a `eth_call` check
before relying on merge].

### 3.5 Gas: EOA transaction, no relayer, no gasless redeem

- `handleTransaction` = `estimateGas` → `tx.wait(1)`, returning `{success, receipt}` [V,
  `OrderBuilder.ts:298-315`]. It is an ordinary transaction **from the signer**, so the signer needs
  BNB. There is **no `relay`, `relayer`, `gasless`, `sponsor`, or paymaster string anywhere in either
  SDK** (grepped both trees: 0 hits) [V].
- The only signing-service-ish thing is the **Gateway**, an on-chain helper the API can authorize:
  `POST /v1/gateway/authorize` (body `{operation, id, amount}` with
  `GatewayOperationKind = SPLIT | MERGE | SPLIT_YES_SET | MERGE_YES_SET | SPLIT_NO_SET | MERGE_NO_SET |
  CONVERT_NO | REDUCE`) and **`POST /v1/gateway/authorize/redeem`** (body
  `{positionIds: string[][], amounts: string[][]}`, max 32 per group) → response
  `{validFrom, refBlock, nonce, signature}` [V, OpenAPI]. But the spec says plainly: *"Returns the
  fields a Gateway call needs … **The caller sends their own transaction**"* and *"The Gateway demands
  this signature only while `signatureRequired()` is on"*. So the Gateway is a signature/nonce
  authorizer, **not a gas sponsor** [V]. Note the redeem call names **`positionIds`** (the per-outcome
  `positionId`s) rather than a `conditionId` — a different granularity from SDK redemption.
- Practical consequence for the backend: keep a small BNB float on the redeeming key, and treat
  "insufficient gas" as a retryable error. If a smart wallet (Kernel) is the maker, gas is paid by
  whatever submits the user-op — outside these SDKs [V/I].

### 3.6 On-chain truth: `payoutDenominator` / `payoutNumerators` — verified by calling BSC

`ConditionalTokens` exposes `payoutDenominator(bytes32) -> uint256` (`0xdd34de67`) and
`payoutNumerators(bytes32, uint256) -> uint256` (`0x0504c814`) [V, ABI + computed selectors].

The contract is **not** `0xC59c…` — it is the `CONDITIONAL_TOKENS` entry of
`AddressesByChainId` (`src/Constants.ts:32`; mirrored in `backend/internal/predictfun/addresses.go`,
where the value is redacted in this session). Verified on-chain: `eth_getCode` returns ~36 KB of code
at that address on **both** chain 56 (mainnet, `0x22DA…d244`) and chain 97 (testnet, `0x2827…7743`) [V].

`eth_call` results for conditions taken from live resolved markets, on the **testnet** CTF [V]:

| market | API `resolution` | `payoutDenominator` | `payoutNumerators[0,1]` |
|---|---|---|---|
| 487 BTC Up/Down | Up (indexSet 1) | 1 | `[1, 0]` |
| 799 (neg-risk, 51-100) | Yes (1) | 1 | `[1, 0]` |
| 798 (neg-risk, 0-50) | No (2) | 1 | `[0, 1]` |
| 1058 | No (2) | 1 | `[0, 1]` |
| **1388 "50-50 event"** | **Yes (1), both outcomes `WON`** | **2** | **`[1, 1]`** → 0.5 each |
| 29076 BTC 09:30-09:45 | Down (2) | 1 | `[0, 1]` |

Payout per share = `payoutNumerators(conditionId, indexSet-1) / payoutDenominator(conditionId)`
(0 → nothing to redeem; 1/1 → 1 USDT per share; 1/2 → 0.50) [I, standard Gnosis CTF math, matches
the two cases above including the 0.5 one].

So: **yes, resolved outcome is readable purely on-chain**, with no API dependency, once you know
`conditionId` (which the API does expose, §2.2). `payoutDenominator == 0` means *not yet determined* —
note that mainnet returned `denominator = 0` for all six of the above condition ids, i.e. this
testnet catalogue's conditions are **not** on mainnet [V]; don't cross-check a testnet condition id
against chain 56.

Neg-risk extra view: `NegRiskAdapter.getDetermined(bytes32)` (`0x7ae2e67b`) and
`getResult(bytes32)` (`0xadd4c784`); both returned `1` for the resolved `cz-tweet-count-december-2025`
category id, and `NegRiskAdapter.getConditionId(bytes32)` (`0x04329c03`) on that same id returned
`0x520823d3…0a54` = **market 798's** `conditionId` (the question at index 0) [V]. Because a neg-risk
category holds one condition *per question*, **take each market's `conditionId` from the API instead
of deriving it from the category**, and treat "which argument `getResult` expects (question id vs
category id)" as **[?]** — the SDK never calls these.

### 3.7 Minimum size

Client-side guard only, in both SDKs: `quantityWei < 1e16` → `InvalidQuantityError`
(`OrderBuilder.ts:506`, `:587`, `:923`; `order_builder.py:237`, `:319`, `:416`) → **0.01 shares**
(1e16 in 18-decimal units) [V]. No API field or on-chain `minSize` was found in the spec [V]; an
exchange-side minimum (if any) is **[?]**. Irrelevant to redemption (redeem any amount > 0), relevant
when you flatten by selling instead of redeeming.

---

## 4. Smart wallet (Kernel / ERC-7579) — how the signature differs

Two independent things change: the **order signature** and the **transaction sender**.

### 4.1 EIP-712 domain for the Kernel-wrapped signature

`src/Constants.ts:27` [V]:

```ts
export const KernelDomainByChainId = {
  [ChainId.BnbMainnet]: { name: "Kernel", version: "0.3.1", chainId: 56 },
  [ChainId.BnbTestnet]: { name: "Kernel", version: "0.3.1", chainId: 97 },
};
```
Python mirror: `constants.py:96 KERNEL_DOMAIN_BY_CHAIN_ID` (same `Kernel` / `0.3.1`) [V].

There is **no literal `verifyingContract` in that object** — it is injected per signer:
`verifyingContract: this.predictAccount` (the smart-wallet address) [V,
`OrderBuilder.ts:906`]. The order's own EIP-712 domain is unchanged and still targets the exchange:
`{ name: "predict.fun CTF Exchange", version: "1", chainId, verifyingContract: <CTF_EXCHANGE or
NEG_RISK_CTF_EXCHANGE …> }` (`Constants.ts:74`, `OrderBuilder.ts:1050-1080`) [V].

### 4.2 The exact digest and signature encoding

`OrderBuilder.ts:897-916` [V]:

```ts
const validatorAddress = this.addresses.ECDSA_VALIDATOR;           // 0x845ADb2C711129d4f3966735eD98a9F09fC4cE57 (both chains)
const kernelDomain    = KernelDomainByChainId[this.chainId];
const messageHash     = typeof message === "string" ? hashMessage(message) : message.raw;
const digest          = eip712WrapHash(messageHash, { ...kernelDomain, verifyingContract: this.predictAccount });
const signedMessage   = await this.signer.signMessage(Buffer.from(digest.slice(2), "hex"));
return concat([concat(["0x01", validatorAddress]), signedMessage]);   // 1 + 20 + 65 = 86 bytes
```

`src/internal/Utils.ts` [V]:

```ts
hashKernelMessage(messageHash) = keccak256(abi.encode(["bytes32","bytes32"],
                                  [keccak256("Kernel(bytes32 hash)"), messageHash]))
eip712WrapHash(messageHash, domain) = keccak256(concat(["0x1901",
                                          TypedDataEncoder.hashDomain(domain), hashKernelMessage(messageHash)]))
```

So, for an order, the signed digest is
`keccak256(0x1901 ‖ EIP712DomainHash(Kernel,0.3.1,chainId,predictAccount) ‖ keccak256(abi.encode(keccak256("Kernel(bytes32 hash)"), orderHash)))`
and the wire `signature` is `0x01 ‖ ECDSA_VALIDATOR(20 bytes) ‖ 65-byte ECDSA sig` — **a 1-byte mode
prefix `0x01` plus the validator address, then the plain signature.** `predictAccount` is set via
`OrderBuilder.make(chainId, signer, { predictAccount })`; the EOA signer is the Privy-exported owner
key (`OrderBuilder.ts:106`), and ownership is checked with
`ECDSAValidator.ecdsaValidatorStorage(predictAccount)` (`OrderBuilder.ts:252-256`) [V].

`signatureType` **is** an order field (`ORDER_STRUCTURE`, `Constants.ts:84`) with
`SignatureType = { EOA: 0, POLY_PROXY: 1, POLY_GNOSIS_SAFE: 2 }` (`Constants.ts:16`, doc-comment
*"EOA also supports EIP-1271"*) [V]. When `predictAccount` is set, `buildOrder` still defaults
`signatureType` to `0` — only `maker` and `signer` are rewritten to the smart-wallet address
(`OrderBuilder.ts:1026-1036`) [V]. So **on the wire a smart-wallet order looks like
`signatureType: 0` + an 86-byte signature**; how the exchange dispatches on length vs. explicit type
is **[?]** (the SDK never sends 1 or 2).

### 4.3 Transactions from a smart wallet

Redemption is wrapped in `Kernel.execute(bytes32 mode, bytes executionCalldata)` with
`mode = ZeroHash` (single-call mode, `OrderBuilder.ts:134`) and
`executionCalldata = abi.encodePacked(target, value(uint256), innerCalldata)` built by
`encodeExecutionCalldata` (`OrderBuilder.ts:328-330`), where `target` is
`NEG_RISK_ADAPTER` / `CONDITIONAL_TOKENS` [V, `redeemPositions` branches at `:697-704` and `:713-720`].

Go equivalent for the backend: since the repo currently refuses `isYieldBearing` markets and does not
sign for smart wallets (`addresses.go` comment: *"This build only signs EOA orders; the smart-wallet
wrapper needs the Kernel domain and a validator contract call, which is not implemented"*), settlement
can stay EOA-only **iff** every maker is an EOA. If a Kernel wallet ever holds the shares, redemption
must go through `Kernel.execute` **sent by the wallet owner** (or a user-op), and the `0x01‖validator‖sig`
encoding above is for *orders*, not for that tx.

---

## 5. What the current Go code gets wrong (action list)

1. `backend/internal/predictfun/client.go:258-268` — `Resolution{Status, WinningIndexSet, ResolvedAt}`
   does not exist on the wire. Real shape: an `Outcome` with `indexSet`, `name`, `status` ∈
   {WON, LOST}, `positionId`. `winningIndexSet`/`resolvedAt` will always decode empty → settlement
   would silently never fire.
2. Add `MarketStatus`/`MarketTradingStatus` enums with the **full** value sets (§2.1), and do not treat
   `PRICE_PROPOSED`/`PRICE_DISPUTED`/`PAUSED` as terminal.
3. Handle the two-WON (draw, denominator 2, 0.50/share) case; `WinningIndexSet` as a scalar cannot
   express it.
4. Settlement decision: `market.status == "RESOLVED"` **and** `outcome.status == "WON"` **and**
   `payoutNumerator > 0` (on-chain) → redeem `[indexSet]` (CTF) or `[amount,0]`/`[0,amount]` (adapter).
5. Redemption needs: `conditionId` + `outcomes[].indexSet` + `market.isNegRisk` + `market.isYieldBearing`
   + `category.negRiskOnChainId` (only for convert) — all obtainable from the API today.
6. `?status=` accepts only `OPEN`/`RESOLVED`; the existing scanner's `Status: "OPEN"` is fine, but a
   settlement poller needs `RESOLVED` (or per-id polling for held markets).
7. `addresses.go` header says the exchange approval + USDT allowance are the only approvals; add
   `NEG_RISK_ADAPTER` ERC-1155 `setApprovalForAll` for neg-risk redeem/merge/convert.
8. `venue-api-contract.md` says `supports_settlement: false` for PF — that can become `true` for the
   redeem path once 1–7 are done; note the proceeds are **collateral (USDT), not a sale**, so the
   trade/PnL record needs a distinct "redeemed" leg.

## 6. Minimal Go redemption recipe (derived from the above)

```
detect:   GET /v1/markets/{id}   -> status=="RESOLVED" && resolution != null
winner:   resolution.indexSet (1|2)  + outcomes[].status for the draw case
payout:   eth_call CTF.payoutDenominator(conditionId) / payoutNumerators(conditionId, idx-1)
amount:   GET /v1/positions?isResolved=true -> position.amount (18-dec string)
if !isNegRisk: tx CTF.redeemPositions(USDT, 0x0, conditionId, [indexSet])
else:          tx NEG_RISK_ADAPTER.redeemPositions(conditionId, [amt,0] or [0,amt])
               (requires CTF.setApprovalForAll(NEG_RISK_ADAPTER,true) first)
guard:    signer needs BNB; no relayer exists; verify receipt.status == 1
confirm:  position.amount drops to 0; /v1/account/activity shows REDEEM
```

## 7. Numbers worth hard-coding (verified)

| item | value |
|---|---|
| CTF redeem selector | `0x01b7037c` |
| CTF merge selector | `0x9e7212ad` |
| CTF `payoutDenominator` / `payoutNumerators` | `0xdd34de67` / `0x0504c814` |
| NegRiskAdapter `redeemPositions` | `0xdbeccb23` |
| NegRiskAdapter `convertPositions` | `0xc64748c4` |
| Kernel `execute` | `0xe9ae5c53`, mode `0x00…00` |
| EIP-712 order domain | `predict.fun CTF Exchange` / `1` |
| Kernel domain | `Kernel` / `0.3.1` / `verifyingContract = predictAccount` |
| smart-wallet signature | `0x01 ‖ ECDSA_VALIDATOR ‖ 65-byte sig` (86 bytes) |
| `ECDSA_VALIDATOR` | `0x845ADb2C711129d4f3966735eD98a9F09fC4cE57` (56 and 97) |
| USDT | `0x55d398326f99059fF775485246999027B3197955` (56) |
| CTF_EXCHANGE / NEG_RISK_CTF_EXCHANGE / NEG_RISK_ADAPTER (56) | `0x8BC070BEdAB741406F4B1Eb65A72bee27894B689` / `0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A` / `0xc3Cf7c252f65E0d8D88537dF96569AE94a7F1A6E` |
| CTF_EXCHANGE / NEG_RISK_CTF_EXCHANGE / NEG_RISK_ADAPTER (97) | `0x2A6413639BD3d73a20ed8C95F634Ce198ABbd2d7` / `0xd690b2bd441bE36431F6F6639D7Ad351e7B29680` / `0x285c1B939380B130D7EBd09467b93faD4BA623Ed` |
| min order quantity (SDK guard) | `1e16` wei = 0.01 shares |

`CONDITIONAL_TOKENS` / `NEG_RISK_CONDITIONAL_TOKENS` (same address, both chains differ from each
other) are redacted in this session's tool output; read them from `src/Constants.ts`
`AddressesByChainId` / `backend/internal/predictfun/addresses.go`. On-chain sanity check used here:
code present at that address on 56 and 97, and `payoutDenominator` returns 1/2 as tabled in §3.6.

## 8. Unknowns (do not guess)

1. **How `onChainId`/`positionId` is derived.** I recomputed the canonical
   `keccak256(collateral ‖ keccak256(0x00…0 ‖ conditionId ‖ indexSet))` for markets 29076, 799 and 487
   with both USDT addresses (and the vault/wcol/CTF candidates) — **no match** with the API's
   `positionId` [V]. The `conditionId` derivation *does* check out (§2.2). Practically irrelevant
   (redeem uses `conditionId`+`indexSet`; balances/orders use the API-supplied `positionId`), but do
   **not** derive token ids yourself. `CTFExchange.getConditionId(tokenId)` returned `0x0` for both
   sampled ids, so it is not a reverse mapping either [V].
2. **`NegRiskAdapter.getResult`/`getDetermined` argument semantics** (question id vs category id) — see
   §3.6; observed `1` on a category id, semantics unconfirmed.
3. **Whether the exchange distinguishes smart-wallet orders by `signatureType` or by signature length.**
4. **Exchange-side minimum size** (only the SDK's client-side `1e16` guard is documented).
5. **Mainnet field drift**: every JSON example here is testnet-hosted. Both hosts serve the same
   OpenAPI version, and the repo already has a working mainnet key path, but the actual mainnet
   payloads for `status`/`resolution` were not observed in this session (401 without a key).
6. **`Protocol: "PREDICT"`** categories: only `LEGACY` was observed on resolved markets; whether
   `PREDICT` implies a different contract set is unknown (the SDK has exactly one set per chain).
