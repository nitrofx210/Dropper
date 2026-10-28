package ethhandler

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	ethprot "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/rlp"
)

// TestStatusRoundTrip verifies a StatusPacket encodes and decodes identically
// across all supported eth protocol versions (69-72). The StatusPacket struct
// is version-independent: only message Length differs per version.
func TestStatusRoundTrip(t *testing.T) {
	genesisHash := common.HexToHash("0xd4e56740f876aef8c010b86a40d5f56745a118d0906a34e69aec8c0db1cb8fa3")
	forkID := forkid.ID{} // zero ID used for parse/encode test only
	latestBlockHash := common.HexToHash("0x5c504ed432cb51138bcf09aa5e8a410dd4a1e204ef84bfed1be16dfba1b22060")

	pkt := &ethprot.StatusPacket{
		ProtocolVersion: 72,
		NetworkID:       1,
		Genesis:         genesisHash,
		ForkID:          forkID,
		EarliestBlock:   0,
		LatestBlock:     12345,
		LatestBlockHash: latestBlockHash,
	}

	enc, err := rlp.EncodeToBytes(pkt)
	if err != nil {
		t.Fatalf("failed to encode StatusPacket: %v", err)
	}

	var dec ethprot.StatusPacket
	if err := rlp.DecodeBytes(enc, &dec); err != nil {
		t.Fatalf("failed to decode StatusPacket: %v", err)
	}

	if dec.ProtocolVersion != pkt.ProtocolVersion {
		t.Errorf("ProtocolVersion mismatch: %d != %d", dec.ProtocolVersion, pkt.ProtocolVersion)
	}
	if dec.NetworkID != pkt.NetworkID {
		t.Errorf("NetworkID mismatch: %d != %d", dec.NetworkID, pkt.NetworkID)
	}
	if dec.Genesis != pkt.Genesis {
		t.Errorf("Genesis mismatch: %x != %x", dec.Genesis, pkt.Genesis)
	}
	if dec.ForkID != pkt.ForkID {
		t.Errorf("ForkID mismatch: %x != %x", dec.ForkID, pkt.ForkID)
	}
	if dec.EarliestBlock != pkt.EarliestBlock {
		t.Errorf("EarliestBlock mismatch: %d != %d", dec.EarliestBlock, pkt.EarliestBlock)
	}
	if dec.LatestBlock != pkt.LatestBlock {
		t.Errorf("LatestBlock mismatch: %d != %d", dec.LatestBlock, pkt.LatestBlock)
	}
	if dec.LatestBlockHash != pkt.LatestBlockHash {
		t.Errorf("LatestBlockHash mismatch: %x != %x", dec.LatestBlockHash, pkt.LatestBlockHash)
	}
}
