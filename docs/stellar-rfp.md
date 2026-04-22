### Prices API

_Added: Q1 2026_

#### **1. Scope of Work**

Develop a publicly accessible API and indexing service to provide normalized price data.

#### **2. Background & Context**

The Stellar ecosystem lacks a single, reliable, and standardized API to source aggregated, real-time, and historical price data for all Stellar assets (both classic and SEP-41 Soroban contract tokens). This fragmentation hinders the development of accurate DeFi protocols, portfolio tracking tools, and general-purpose applications, increasing integration complexity for builders.

#### **3. Requirements**

* Asset Coverage: Support for all current native Stellar assets and SEP-41 Soroban contract tokens.&#x20;
* Oracle Coverage: Support for oracles, including but not limited to prices streamed from Chainlink, Redstone, Band, Reflector, and others.
* Price Aggregation: Calculate a weighted average price across major on-chain and off-chain markets (e.g., Soroswap, Aquarius, SDEX, Blend, etc.).&#x20;
* Adjustable Volume Threshold: At least one version should allow prices to be weighted by trading volume (VWAP), requiring a configurable USD-denominated trading volume threshold for market inclusion to filter out illiquid pairs.
* Data Endpoints: Real-time price endpoints, and historical price endpoints for different intervals (e.g., 24h, 7d, 30d, 1yr) for charting price changes over time.&#x20;
* Base and quote volume in USD.
* OHLC: Historical price endpoints should offer OHLC aggregates to support candlestick charts and their use cases.
* Supported timeframes: 1 hour, 24 hours, 1 week, 1 month, 1 year, and All-Time (since asset inception)
* Suggested granularity: 1 min, 15 min, 1 hour, 4 hours, 1 day, 1 week, 1 month
* Granularities can be matched to timeframes and finer granularities (1 min to 4 hours) do not have to be stored forever. The granularities of 1 hour and above must remain available indefinitely for the All-Time view.
* Performance: Must be highly available, low-latency, and capable of handling high query volume. Teams must clearly explain what happens when prices become unavailable, sources start to differ, etc.
* Repo needs to be completely Open Source (Tranche I and II)

**Asset Metadata**

| Field            | Type           | Source      | Notes                          |
| ---------------- | -------------- | ----------- | ------------------------------ |
| Asset/Token Code | string         | On-chain    | Asset name                     |
| Current Price    | float (USD)    | Pricing API | Real-time or near-real-time    |
| Asset Type       | enum           | On-chain    | classic \| soroban             |
| Issuer Address   | string         | On-chain    | Issuer public key (G...)       |
| Contract Address | string         | On-chain    | Issuer contract address (C...) |
| Home Domain      | string \| null | On-chain    | Classic assets only; nullable  |

**Historical Price Chart**

| Timeframe       | Granularity (suggested) | Data Points | Price Type   |
| --------------- | ----------------------- | ----------- | ------------ |
| 1 hour          | 1 min                   | \~60        | TWAP or VWAP |
| 24 hours        | 15 min                  | \~96        | TWAP or VWAP |
| 1 week          | 1 hr                    | \~168       | TWAP or VWAP |
| 1 month         | 4 hr                    | \~180       | TWAP or VWAP |
| Since Inception | 1 day                   | Variable    | TWAP or VWAP |

#### **4. Evaluation Criteria**

* Technical design quality
* Relevant experience in building performant, low latency APIs
* Security: Resistant to relevant attack vectors
* Ecosystem alignment: Proactive in understanding and indexing prices for classic and Soroban assets
* Ability to deliver within 2 months
* Coherent integration plan

#### **5. Expected Deliverables**

* Ready to use API production deployment within \~10 weeks from first award payment.&#x20;
* API reference documentation
* Self-service onboarding experience
* Examples:
  * [https://stellar.expert](https://stellar.expert)
  * [https://steexp.com/](https://steexp.com/)&#x20;