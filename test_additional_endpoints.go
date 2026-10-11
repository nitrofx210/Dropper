package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Simple RPC endpoint tester
type RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      uint64        `json:"id"`
}

type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      uint64          `json:"id"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func testEndpoint(name, url string) {
	fmt.Printf("Testing %s (%s)... ", name, url)

	client := &http.Client{Timeout: 10 * time.Second}

	// Test eth_blockNumber
	req := RPCRequest{
		JSONRPC: "2.0",
		Method:  "eth_blockNumber",
		Params:  []interface{}{},
		ID:      1,
	}

	reqBody, _ := json.Marshal(req)
	resp, err := client.Post(url, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		fmt.Printf("FAILED: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Printf("HTTP %d\n", resp.StatusCode)
		return
	}

	var rpcResp RPCResponse
	err = json.NewDecoder(resp.Body).Decode(&rpcResp)
	if err != nil {
		fmt.Printf("DECODE ERROR: %v\n", err)
		return
	}

	if rpcResp.Error != nil {
		fmt.Printf("RPC ERROR: %v\n", rpcResp.Error)
		return
	}

	fmt.Printf("OK - Block: %s\n", string(rpcResp.Result))
}

func main() {
	endpoints := []struct {
		name string
		url  string
	}{
		{"Etherscan", "https://api.etherscan.io/api?module=proxy&action=eth_blockNumber&apikey=YourApiKeyToken"},
		{"Alchemy (free tier)", "https://eth-mainnet.alchemyapi.io/v2/demo"},
		{"Infura (free tier)", "https://mainnet.infura.io/v3/YOUR-PROJECT-ID"},
		{"DRPC (re-test)", "https://eth.drpc.org"},
	}

	for _, ep := range endpoints {
		testEndpoint(ep.name, ep.url)
	}
}