package observer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/kingd/ethereum-dropper-lab/internal/config"
	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

// TestParseNodeMainnetBootnodes covers all 9 Mainnet bootnode entries:
// 5 raw ENR records ("enr:...") via enode.Parse and 4 legacy "enode://"
// URLs via enode.ParseV4.
func TestParseNodeMainnetBootnodes(t *testing.T) {
	var enrEntries, legacyEntries []string
	for _, bn := range params.MainnetBootnodes {
		if strings.HasPrefix(bn, "enr:") {
			enrEntries = append(enrEntries, bn)
		} else {
			legacyEntries = append(legacyEntries, bn)
		}
	}
	if len(enrEntries) == 0 {
		t.Fatal("expected ENR fixtures in MainnetBootnodes")
	}
	if len(legacyEntries) == 0 {
		t.Fatal("expected legacy fixtures in MainnetBootnodes")
	}

	// ENR records parse via enode.Parse (dispatch branch).
	for _, bn := range enrEntries {
		node, err := parseNode(bn)
		if err != nil {
			t.Errorf("ENR bootnode should parse %q: %v", bn, err)
			continue
		}
		if node == nil {
			t.Errorf("ENR bootnode returned nil node: %q", bn)
			continue
		}
		if node.ID() == (enode.ID{}) {
			t.Errorf("ENR bootnode returned empty node ID: %q", bn)
		}
	}

	// Legacy enode:// URLs parse via enode.ParseV4 (dispatch branch).
	for _, bn := range legacyEntries {
		node, err := parseNode(bn)
		if err != nil {
			t.Errorf("legacy bootnode should parse %q: %v", bn, err)
			continue
		}
		if node == nil {
			t.Errorf("legacy bootnode returned nil node: %q", bn)
			continue
		}
		if node.ID() == (enode.ID{}) {
			t.Errorf("legacy bootnode returned empty node ID: %q", bn)
		}
	}

	if len(params.MainnetBootnodes) != 9 {
		t.Errorf("expected 9 Mainnet bootnodes, got %d", len(params.MainnetBootnodes))
	}
}

// TestParseNodeMalformedEntries verifies malformed entries are skipped
// cleanly (error returned, no panic) and do not block valid entries.
func TestParseNodeMalformedEntries(t *testing.T) {
	malformed := []string{
		"",
		"   ",
		"not-a-node-id",
		"enr:notvalidbase64!!!",
		"enode://",           // malformed legacy URL
		"ftp://something",    // unknown scheme
		"enode://@@bad@host", // garbage legacy URL
	}
	for _, s := range malformed {
		_, err := parseNode(s)
		if err == nil {
			t.Errorf("malformed entry should error: %q", s)
		}
	}
}

// TestParseNodeBareHexNodeID verifies bare hex node IDs parse via ParseV4.
func TestParseNodeBareHexNodeID(t *testing.T) {
	// A full 64-hex-char secp256k1 public key from a legacy Mainnet bootnode.
	bare := "d860a01f9722d78051619d1e2351aba3f43f943f6f00718d1b9baa4101932a1f5011f16bb2b1bb35db20d6fe28fa0bf09636d26a87d31de9ec6203eeedb1f666"

	node, err := parseNode(bare)
	if err != nil {
		t.Fatalf("bare hex node ID should parse: %v", err)
	}
	if node == nil {
		t.Fatal("bare hex node ID returned nil node")
	}
	if node.ID() == (enode.ID{}) {
		t.Error("bare hex node ID returned empty node ID")
	}
}

// TestParseNodeDispatchCorrectParser exercises both dispatch branches.
func TestParseNodeDispatchCorrectParser(t *testing.T) {
	enr := params.MainnetBootnodes[0]    // first entry is an ENR record
	legacy := params.MainnetBootnodes[5] // sixth entry is an enode:// URL

	node, err := parseNode(enr)
	if err != nil {
		t.Fatalf("ENR should parse via enode.Parse: %v", err)
	}
	if node == nil || node.ID() == (enode.ID{}) {
		t.Error("ENR returned nil node or empty node ID")
	}

	node, err = parseNode(legacy)
	if err != nil {
		t.Fatalf("legacy URL should parse via enode.ParseV4: %v", err)
	}
	if node == nil || node.ID() == (enode.ID{}) {
		t.Error("legacy URL returned nil node or empty node ID")
	}
}

// TestObserverDialRatioConfig verifies that the Observer's p2p.Server
// config carries the configured DialRatio from the config file.
func TestObserverDialRatioConfig(t *testing.T) {
	// Test with DialRatio=3
	cfg := config.MainnetConfig()
	cfg.DialRatio = 3
	cfg.StoragePath = "file::memory:?cache=shared"
	cfg.ObserverID = "test-observer"
	cfg.GethLibVersion = "test"
	cfg.GoBuildVersion = "test"
	cfg.CollectorGitCommit = "test"
	cfg.ConfigHash = "test"
	cfg.SchemaVersion = "1"

	sto, err := storage.New(cfg.StoragePath)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	defer sto.Close()

	// Create Observer (this calls setupServer internally)
	o, err := New(cfg, "test_config.json")
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}

	// Start the observer briefly to initialize the p2p server
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = o.Start(ctx)
	// Start returns error when context is cancelled, which is expected
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("observer.Start: %v", err)
	}

	// Verify the server's config has the correct DialRatio
	if o.server == nil {
		t.Fatal("server not initialized")
	}
	if o.server.Config.DialRatio != 3 {
		t.Errorf("expected server.Config.DialRatio=3, got %d", o.server.Config.DialRatio)
	}

	// Verify effective dial limits
	maxDialed := o.server.MaxDialedConns()
	maxInbound := o.server.MaxInboundConns()
	expectedDialed := cfg.MaxPeers / cfg.DialRatio   // 50/3 = 16
	expectedInbound := cfg.MaxPeers - expectedDialed // 50-16 = 34
	if maxDialed != expectedDialed {
		t.Errorf("expected MaxDialedConns=%d, got %d", expectedDialed, maxDialed)
	}
	if maxInbound != expectedInbound {
		t.Errorf("expected MaxInboundConns=%d, got %d", expectedInbound, maxInbound)
	}

	// Test with default DialRatio=2
	cfg2 := config.MainnetConfig()
	cfg2.StoragePath = "file::memory:?cache=shared"
	cfg2.ObserverID = "test-observer-2"
	cfg2.GethLibVersion = "test"
	cfg2.GoBuildVersion = "test"
	cfg2.CollectorGitCommit = "test"
	cfg2.ConfigHash = "test"
	cfg2.SchemaVersion = "1"
	// DialRatio defaults to 2 in MainnetConfig()

	sto2, err := storage.New(cfg2.StoragePath)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	defer sto2.Close()

	o2, err := New(cfg2, "test_config2.json")
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}

	// Start the observer briefly to initialize the p2p server
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	err = o2.Start(ctx2)
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("observer.Start: %v", err)
	}

	if o2.server == nil {
		t.Fatal("server not initialized")
	}
	if o2.server.Config.DialRatio != 2 {
		t.Errorf("expected server.Config.DialRatio=2 (default), got %d", o2.server.Config.DialRatio)
	}

	maxDialed2 := o2.server.MaxDialedConns()
	maxInbound2 := o2.server.MaxInboundConns()
	expectedDialed2 := cfg2.MaxPeers / 2                // 50/2 = 25
	expectedInbound2 := cfg2.MaxPeers - expectedDialed2 // 50-25 = 25
	if maxDialed2 != expectedDialed2 {
		t.Errorf("expected MaxDialedConns=%d, got %d", expectedDialed2, maxDialed2)
	}
	if maxInbound2 != expectedInbound2 {
		t.Errorf("expected MaxInboundConns=%d, got %d", expectedInbound2, maxInbound2)
	}
}
