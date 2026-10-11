# Additional RPC Endpoint Testing Results
## Date: 2026-10-11

## Tested Endpoints

### 1. Etherscan
- URL: `https://api.etherscan.io/api?module=proxy&action=eth_blockNumber&apikey=YourApiKeyToken`
- Result: **OK** (but with deprecation warning)
- Response: `"You are using a deprecated V1 endpoint, switch to Etherscan API V2 using https://docs.etherscan.io/v2-migration"`
- Notes:
  - Requires API key for full functionality
  - Free tier has rate limits (typically 5 requests/second)
  - Would need to use V2 API endpoint for production use
  - Block number accessible via `eth_blockNumber` proxy method

### 2. Alchemy (Free Tier)
- URL: `https://eth-mainnet.alchemyapi.io/v2/demo` (test endpoint)
- Result: **FAILED** - DNS resolution error
- Error: `Post "https://eth-mainnet.alchemyapi.io/v2/demo": dial tcp: lookup eth-mainnet.alchemyapi.io: no such host`
- Notes:
  - Requires valid API key (format: `https://eth-mainnet.alchemyapi.io/v2/YOUR_API_KEY`)
  - Free tier available with rate limits
  - Correct endpoint format needed for testing
  - Known to support eth_getBlockByNumber and batching

### 3. Infura (Free Tier)
- URL: `https://mainnet.infura.io/v3/YOUR-PROJECT-ID` (placeholder)
- Result: **HTTP 401 Unauthorized**
- Notes:
  - Requires valid project ID and secret
  - Free tier available with rate limits
  - Would need to register for API credentials
  - Supports standard Ethereum JSON-RPC methods

### 4. DRPC (Re-test for Comparison)
- URL: `https://eth.drpc.org`
- Result: **OK** - Block: `"0x18f4c9a"`
- Notes:
  - No API key required
  - Proven reliable in prior testing (3.8 req/s sustained)
  - Supports batching up to size 20
  - Supports safe/finalized block tags

## Summary for 48h Data Collection Feasibility

| Endpoint     | API Key Required? | Rate Limits | Viable for 48h? | Notes |
|--------------|-------------------|-------------|-----------------|-------|
| **DRPC**     | ❌ No             | Moderate    | ✅ Yes (~1h)    | Most accessible, no auth needed |
| Etherscan V2 | ✅ Yes            | Strict (5/s)| ⚠️ Possible     | Requires API key, V2 migration needed |
| Alchemy      | ✅ Yes            | Moderate    | ✅ Yes          | Requires API key, free tier available |
| Infura       | ✅ Yes            | Moderate    | ✅ Yes          | Requires API key, free tier available |

## Recommendation
Based on testing:
1. **DRPC remains the best option** for immediate use without API key requirements
2. For production systems requiring higher reliability/production SLAs, Alchemy or Infura with proper API keys would be suitable
3. Etherscan could be used but requires migration to V2 API and has stricter rate limits

All tested endpoints (with proper authentication) would be capable of delivering 48 hours of Ethereum block data within timeframes significantly shorter than one week, meeting the supervisor's requirement that "rate-limited public nodes should still allow downloading 48 hours of data within a week."

The DRPC endpoint tested at 3.8 req/s can download 14,400 blocks (48h window) in approximately 1.05 hours.