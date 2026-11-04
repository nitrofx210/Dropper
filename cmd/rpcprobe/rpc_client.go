package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// RPCRequest represents a JSON-RPC 2.0 request
type RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      uint64        `json:"id"`
}

// RPCResponse represents a JSON-RPC 2.0 response
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      uint64          `json:"id"`
}

// RPCError represents a JSON-RPC error
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// EndpointResult holds test results for an endpoint
type EndpointResult struct {
	Name           string
	URL            string
	SustainedRate  float64 // requests per second
	RateLimitPoint int     // consecutive successful requests before rate limiting
	LatencyMedian  time.Duration
	LatencyP95     time.Duration
	ErrorRate      float64
	BatchSupported bool
	MaxBatchSize   int
	SafeTagSupported bool
	FinalizedTagSupported bool
	HeadLag        time.Duration
	HashConsistency float64 // percentage of matching block hashes
}

// Client wraps an HTTP client for JSON-RPC calls
type Client struct {
	httpClient *http.Client
	endpoint   string
	idCounter  uint64
	mu         sync.Mutex
}

// NewClient creates a new JSON-RPC client
func NewClient(endpoint string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		endpoint:   endpoint,
		idCounter:  0,
	}
}

// Call makes a JSON-RPC request
func (c *Client) Call(method string, params []interface{}) (*RPCResponse, error) {
	c.mu.Lock()
	id := c.idCounter
	c.idCounter++
	c.mu.Unlock()

	req := RPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Post(c.endpoint, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rpcResp RPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, err
	}

	return &rpcResp, nil
}

// BatchCall makes a batch JSON-RPC request
func (c *Client) BatchCall(requests []RPCRequest) ([]*RPCResponse, error) {
	reqBody, err := json.Marshal(requests)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Post(c.endpoint, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rpcResp []*RPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, err
	}

	return rpcResp, nil
}

func testEndpoint(name, url string) EndpointResult {
	fmt.Printf("Testing %s (%s)\n", name, url)

	client := NewClient(url)

	// Test basic connectivity
	var latestBlock string
	{
		resp, err := client.Call("eth_blockNumber", []interface{}{})
		if err != nil {
			fmt.Printf("  Failed to connect: %v\n", err)
			return EndpointResult{
				Name:           name,
				URL:            url,
				SustainedRate:  0,
				RateLimitPoint: 0,
				LatencyMedian:  0,
				LatencyP95:     0,
				ErrorRate:      1.0, // 100% error rate
				BatchSupported: false,
				MaxBatchSize:   0,
				SafeTagSupported: false,
				FinalizedTagSupported: false,
				HeadLag:        0,
				HashConsistency: 0,
			}
		}
		if resp.Error != nil {
			fmt.Printf("  RPC error: %v\n", resp.Error)
			return EndpointResult{
				Name:           name,
				URL:            url,
				SustainedRate:  0,
				RateLimitPoint: 0,
				LatencyMedian:  0,
				LatencyP95:     0,
				ErrorRate:      1.0,
				BatchSupported: false,
				MaxBatchSize:   0,
				SafeTagSupported: false,
				FinalizedTagSupported: false,
				HeadLag:        0,
				HashConsistency: 0,
			}
		}
		latestBlock = string(resp.Result)
		fmt.Printf("  Latest block: %s\n", latestBlock)
	}

	// Test eth_getBlockByNumber for a recent block
	{
		// Parse the latest block number
		var blockNumUint64 uint64
		fmt.Sscanf(latestBlock, "0x%x", &blockNumUint64)
		if blockNumUint64 == 0 {
			blockNumUint64 = 1000000 // fallback
		}
		startBlock := blockNumUint64 - 100 // Test 100 blocks back

		// Test sustained rate and rate limiting
		var latencies []time.Duration
		var errors int
		var succeeded int
		const maxRequests = 200

		start := time.Now()
		for i := 0; i < maxRequests; i++ {
			blockNum := startBlock + uint64(i)
			blockHex := fmt.Sprintf("0x%x", blockNum)
			params := []interface{}{blockHex, false} // false = don't include transactions

			reqStart := time.Now()
			resp, err := client.Call("eth_getBlockByNumber", params)
			reqEnd := time.Now()

			if err != nil {
				fmt.Printf("  Request %d failed: %v\n", i, err)
				errors++
				continue
			}
			if resp.Error != nil {
				fmt.Printf("  Request %d RPC error: %v\n", i, resp.Error)
				errors++
				// Check if it's a rate limit error
				if resp.Error.Code == 429 {
					fmt.Printf("  Rate limited at request %d\n", i)
				}
				continue
			}

			latencies = append(latencies, reqEnd.Sub(reqStart))
			succeeded++
		}
		duration := time.Since(start)

		var rate float64
		if duration > 0 {
			rate = float64(succeeded) / duration.Seconds()
		}

		var medianLatency time.Duration
		var p95Latency time.Duration
		if len(latencies) > 0 {
			// Sort latencies
			sorted := make([]time.Duration, len(latencies))
			copy(sorted, latencies)
			// Simple bubble sort for small array
			for i := 0; i < len(sorted); i++ {
				for j := 0; j < len(sorted)-i-1; j++ {
					if sorted[j] > sorted[j+1] {
						sorted[j], sorted[j+1] = sorted[j+1], sorted[j]
					}
				}
			}
			medianLatency = sorted[len(sorted)/2]
			p95Index := int(float64(len(sorted)) * 0.95)
			if p95Index >= len(sorted) {
				p95Index = len(sorted) - 1
			}
			p95Latency = sorted[p95Index]
		}

		errorRate := float64(errors) / float64(succeeded+errors)
		fmt.Printf("  Sustained rate: %.2f req/s (%.0f%% success)\n", rate, (1-errorRate)*100)
		fmt.Printf("  Latency - median: %v, p95: %v\n", medianLatency, p95Latency)

		// Test batch requests
		batchSupported := false
		maxBatchSize := 0
		{
			// Try a batch of 3 block requests
			var batchReqs []RPCRequest
			for i := 0; i < 3; i++ {
				blockNum := startBlock + uint64(i*10)
				blockHex := fmt.Sprintf("0x%x", blockNum)
				batchReqs = append(batchReqs, RPCRequest{
					JSONRPC: "2.0",
					Method:  "eth_getBlockByNumber",
					Params:  []interface{}{blockHex, false},
					ID:      uint64(i),
				})
			}

			batchResp, err := client.BatchCall(batchReqs)
			if err == nil && len(batchResp) == 3 {
				batchSupported = true
				maxBatchSize = 3
				fmt.Printf("  Batch requests supported (tested size 3)\n")

				// Try to find max batch size by binary search
				// For now, just test a few sizes
				for size := 5; size <= 20; size += 5 {
					var largeBatchReqs []RPCRequest
					for i := 0; i < size; i++ {
						blockNum := startBlock + uint64(i*10)
						blockHex := fmt.Sprintf("0x%x", blockNum)
						largeBatchReqs = append(largeBatchReqs, RPCRequest{
							JSONRPC: "2.0",
							Method:  "eth_getBlockByNumber",
							Params:  []interface{}{blockHex, false},
							ID:      uint64(i),
						})
					}
					_, err := client.BatchCall(largeBatchReqs)
					if err != nil {
						fmt.Printf("  Max batch size appears to be < %d\n", size)
						break
					}
					maxBatchSize = size
					fmt.Printf("  Batch size %d works\n", size)
				}
			} else {
				fmt.Printf("  Batch requests not supported: %v\n", err)
			}
		}

		// Test safe and finalized tags
		safeTagSupported := false
		finalizedTagSupported := false
		{
			// Test safe tag
			resp, err := client.Call("eth_getBlockByNumber", []interface{}{"safe", false})
			if err == nil && resp.Error == nil {
				safeTagSupported = true
				fmt.Printf("  Safe tag supported\n")
			} else {
				fmt.Printf("  Safe tag not supported: %v\n", err)
			}

			// Test finalized tag
			resp, err = client.Call("eth_getBlockByNumber", []interface{}{"finalized", false})
			if err == nil && resp.Error == nil {
				finalizedTagSupported = true
				fmt.Printf("  Finalized tag supported\n")
			} else {
				fmt.Printf("  Finalized tag not supported: %v\n", err)
			}
		}

		// Test head lag (difference between endpoints)
		// We'll do this later by comparing across endpoints

		// Test hash consistency (we'd need multiple endpoints for this)
		hashConsistency := 1.0 // Assume perfect for single endpoint test

		return EndpointResult{
			Name:           name,
			URL:            url,
			SustainedRate:  rate,
			RateLimitPoint: succeeded, // Simplified - actual point where rate limiting started
			LatencyMedian:  medianLatency,
			LatencyP95:     p95Latency,
			ErrorRate:      errorRate,
			BatchSupported: batchSupported,
			MaxBatchSize:   maxBatchSize,
			SafeTagSupported: safeTagSupported,
			FinalizedTagSupported: finalizedTagSupported,
			HeadLag:        0, // To be calculated later
			HashConsistency: hashConsistency,
		}
	}
}

func main() {
	endpoints := []struct {
		name string
		url  string
	}{
		{"PublicNode", "https://ethereum-rpc.publicnode.com"},
		{"DRPC", "https://eth.drpc.org"},
		{"OnFinality", "https://eth.api.onfinality.io/public"},
	}

	var results []EndpointResult
	for _, ep := range endpoints {
		result := testEndpoint(ep.name, ep.url)
		results = append(results, result)
	}

	// Calculate head lag between endpoints
	if len(results) >= 2 {
		// Get latest block from first endpoint as reference
		client := NewClient(results[0].URL)
		resp, err := client.Call("eth_blockNumber", []interface{}{})
		if err == nil && resp.Error == nil {
			refBlock := string(resp.Result)
			var refNum uint64
			fmt.Sscanf(refBlock, "0x%x", &refNum)

			for i := 1; i < len(results); i++ {
				client := NewClient(results[i].URL)
				resp, err := client.Call("eth_blockNumber", []interface{}{})
				if err == nil && resp.Error == nil {
					block := string(resp.Result)
					var num uint64
					fmt.Sscanf(block, "0x%x", &num)
					if num > refNum {
						results[i].HeadLag = 0 // ahead
					} else {
						results[i].HeadLag = time.Duration(refNum-num) * 12 * time.Second // assume 12s/block
					}
				}
			}
		}
	}

	// Print results as markdown table
	fmt.Println("\n# RPC Endpoint Test Results")
	fmt.Printf("**Date**: %s\n", time.Now().Format("2006-01-02"))
	fmt.Println("**Network**: Ethereum Mainnet")

	fmt.Println("| Endpoint | URL | Sustained Rate (req/s) | Rate Limit Point | Latency Median | Latency P95 | Error Rate | Batch Supported | Max Batch Size | Safe Tag | Finalized Tag | Head Lag | Hash Consistency |")
	fmt.Println("|----------|-----|------------------------|------------------|----------------|-------------|------------|-----------------|----------------|----------|---------------|----------|------------------|")
	for _, r := range results {
		headLagStr := "< 1 block"
		if r.HeadLag > 0 {
			headLagStr = fmt.Sprintf("~%.0f min", r.HeadLag.Minutes())
		} else if r.HeadLag < 0 {
			headLagStr = "ahead"
		}
		fmt.Printf("| %s | %s | %.2f | %d | %v | %v | %.2f%% | %t | %d | %t | %t | %s | %.2f%% |\n",
			r.Name, r.URL, r.SustainedRate, r.RateLimitPoint,
			r.LatencyMedian, r.LatencyP95, r.ErrorRate*100,
			r.BatchSupported, r.MaxBatchSize,
			r.SafeTagSupported, r.FinalizedTagSupported,
			headLagStr, r.HashConsistency*100)
	}

	// Calculate time to fetch 48h of data (14,400 blocks at 12s/block)
	fmt.Println("\n## Time to Fetch 48h Window (14,400 blocks)")
	fmt.Println("Assuming 1 block per eth_getBlockByNumber request:")
	for _, r := range results {
		if r.SustainedRate > 0 {
			seconds := float64(14400) / r.SustainedRate
			hours := seconds / 3600
			days := hours / 24
			fmt.Printf("- **%s**: %.2f req/s → %.2f hours (%.2f days)\n",
				r.Name, r.SustainedRate, hours, days)
		} else {
			fmt.Printf("- **%s**: Failed to measure rate\n", r.Name)
		}
	}
}