package storage

import (
	"time"
)

// PeerEvent represents a peer lifecycle event
type PeerEvent struct {
	ID           int64
	RunID        int64
	ObserverID   string
	PeerID       string // enode ID hex
	EventType    string // connect, disconnect, handshake_ok
	Direction    string // inbound, outbound
	Timestamp    time.Time
	ClientName   string
	ClientVersion string
	Enode        string
	RemoteAddr   string
	EthVersion   int
	DetailJSON   string
}

// TxObservation represents a transaction observation from a peer
type TxObservation struct {
	ID              int64
	RunID           int64
	ObserverID      string
	PeerID          string // enode ID hex
	TxHash          []byte
	Timestamp       time.Time
	MessageType     string // announcement, direct_broadcast, pooled_tx_response
	EthVersion      int
	TxType          uint8
	AnnouncedSize   uint32
	RawTransaction  []byte
	SourceConfidence string // direct, inferred, unknown
	CustodyMask     []byte
}

// CollectionRun represents a single observer run
type CollectionRun struct {
	ID               int64
	StartTime        time.Time
	EndTime          time.Time
	ObserverID       string
	GethLibVersion   string
	GoBuildVersion   string
	CollectorGitCommit string
	ConfigHash       string
	SchemaVersion    string
	Mode             string // passive, fetch
	DataLabel        string // MAINNET_OBSERVATION, LOCAL_TESTNET, CONTROLLED_SIMULATION
	Notes            string
}

// TrackedTransaction represents a transaction that we are tracking for inclusion
type TrackedTransaction struct {
	TxHash          []byte
	FirstSeenNs     int64
	LastCheckedNs   int64
	NextCheckNs     int64
	Attempt         int
	Included        *bool   // nil if unknown, true if included, false if not included (and we have given up or max attempts)
	BlockNumber     *int64
	BlockHash       []byte
	TransactionIndex *int64
	UsefulScore     *float64
}