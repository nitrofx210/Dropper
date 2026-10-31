package observer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nat"
	"github.com/ethereum/go-ethereum/params"
	"runtime/debug"

	"github.com/kingd/ethereum-dropper-lab/internal/config"
	"github.com/kingd/ethereum-dropper-lab/internal/observer/ethhandler"
	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

const chainRefreshInterval = 60 * time.Second

// rateLimiter is a simple token bucket limiter for rate limiting events.
type rateLimiter struct {
	interval time.Duration
	lastTime time.Time
	mutex    sync.Mutex
}

// newRateLimiter returns a new rateLimiter that allows up to 'rate' events per second.
func newRateLimiter(rate float64) *rateLimiter {
	if rate <= 0 {
		return nil
	}
	return &rateLimiter{
		interval: time.Duration(float64(time.Second) / rate),
		lastTime: time.Now(),
	}
}

// take returns true if the limiter allows an event to happen now.
// It updates the lastTime to now if allowed.
func (r *rateLimiter) take() bool {
	if r == nil {
		return true
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	now := time.Now()
	if now.Sub(r.lastTime) >= r.interval {
		r.lastTime = now
		return true
	}
	return false
}

// Observer represents the core observer functionality
type Observer struct {
	cfg        *config.Config
	storage    *storage.Storage
	ethHandler *ethhandler.EthHandler
	server     *p2p.Server
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	// peerDir records whether each known peer is outbound (true) or inbound (false).
	// Populated from the eth handler via handlePeerDirection, which has the actual
	// *p2p.Peer (and thus p.Inbound()). Direction for connect/disconnect peer events
	// is looked up here so discovery-dialed peers are reported outbound instead of
	// being mislabeled inbound via the bootnode set.
	peerDirMu sync.Mutex
	peerDir   map[enode.ID]bool

	// stats tracks runtime counters for periodic logging.
	startTime time.Time
	runID     int64
}

// New creates a new Observer instance
func New(cfg *config.Config) (*Observer, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}

	// Initialize storage
	sto, err := storage.New(cfg.StoragePath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}

	// Initialize eth handler. runID/observerID are placeholder values here;
	// the real values are threaded in via SetRunContext once Start() creates
	// the collection run.
	ethHandler := ethhandler.New(sto, 0, "", cfg.Mode == "fetch", cfg.TxDisseminationEnabled, cfg.TxDisseminationRateLimit)

	// Create observer
	obs := &Observer{
		cfg:        cfg,
		storage:    sto,
		ethHandler: ethHandler,
	}

	return obs, nil
}

// Start begins the observer's operation
func (o *Observer) Start(ctx context.Context) error {
	// Configure the log level from config before anything else so early
	// messages (e.g. NAT dial errors) appear at the requested verbosity.
	o.setupLogLevel()

	o.ctx, o.cancel = context.WithCancel(ctx)
	defer o.cancel()

	// Initialize p2p server
	bootnodesOK, bootnodesSkipped, staticNodesOK, staticNodesSkipped, err := o.initP2PServer()
	if err != nil {
		return fmt.Errorf("failed to initialize p2p server: %w", err)
	}

	// Startup summary: configuration and bootnode parse results.
	o.startTime = time.Now()
	log.Info("Observer starting",
		"listen_addr", o.cfg.ListenAddr,
		"max_peers", o.cfg.MaxPeers,
		"no_discovery", o.cfg.NoDiscovery,
		"nat", o.cfg.NAT,
		"mode", o.cfg.Mode,
		"tx_dissemination_enabled", o.cfg.TxDisseminationEnabled,
		"tx_dissemination_rate_limit", o.cfg.TxDisseminationRateLimit,
		"storage_path", o.cfg.StoragePath,
		"bootnodes_total", len(o.cfg.Bootnodes),
		"bootnodes_parsed", bootnodesOK,
		"bootnodes_skipped", bootnodesSkipped,
		"static_nodes_total", len(o.cfg.StaticNodes),
		"static_nodes_parsed", staticNodesOK,
		"static_nodes_skipped", staticNodesSkipped,
	)

	// Populate run metadata from the build (collector_git_commit) and the
	// actual config bytes (config_hash = sha256 of config file), logging
	// both at startup per review requirement D/E.
	configFile := "configs/real_run.json"
	o.cfg.CollectorGitCommit = gitCommit()
	o.cfg.ConfigHash = configHash(configFile)
	log.Info("Observer metadata",
		"config_file", configFile,
		"collector_git_commit", o.cfg.CollectorGitCommit,
		"config_hash", o.cfg.ConfigHash,
		"go_version", o.cfg.GoBuildVersion,
		"geth_lib_version", o.cfg.GethLibVersion,
		"observer_id", o.cfg.ObserverID,
	)

	// Start a collection run row and record runID for labeling observation output
	now := time.Now()
	runID, err := o.storage.StartCollectionRun(&storage.CollectionRun{
		StartTime:          now,
		ObserverID:         o.cfg.ObserverID,
		GethLibVersion:     o.cfg.GethLibVersion,
		GoBuildVersion:     o.cfg.GoBuildVersion,
		CollectorGitCommit: o.cfg.CollectorGitCommit,
		ConfigHash:         o.cfg.ConfigHash,
		SchemaVersion:      o.cfg.SchemaVersion,
		Mode:               o.cfg.Mode,
		DataLabel:          o.cfg.DataLabel,
		Notes:              o.cfg.Notes,
	})
	if err != nil {
		return fmt.Errorf("failed to start collection run: %w", err)
	}

	log.Info("Collection run started", "run_id", runID, "storage_path", o.cfg.StoragePath)
	o.runID = runID

	// Point 1: Compute ForkID from the live head block via RPC and log the
	// eth Status handshake contents, labelled EXTERNAL.
	if err := o.logStatusHandshake(ctx); err != nil {
		log.Warn("Could not compute/log eth Status handshake ForkID", "err", err)
	}

	// Thread the real run ID and observer ID into the eth handler so all
	// observations and peer events are labelled with the active collection run.
	o.ethHandler.SetRunContext(runID, o.cfg.ObserverID)

	// Report each peer's actual direction (p.Inbound()) to the observer so
	// connect/disconnect peer events are accurate (Task 3).
	o.ethHandler.SetPeerDirCallback(func(id enode.ID, outbound bool) {
		o.peerDirMu.Lock()
		o.peerDir[id] = outbound
		o.peerDirMu.Unlock()
	})

	// Start server
	if err := o.server.Start(); err != nil {
		return fmt.Errorf("failed to start p2p server: %w", err)
	}
	defer o.server.Stop()

	// Subscribe to peer lifecycle events (connect/disconnect) and persist them so
	// disconnect reasons are recorded alongside the collection run.
	eventCh := make(chan *p2p.PeerEvent, 64)
	sub := o.server.SubscribeEvents(eventCh)
	defer sub.Unsubscribe()

	// Peer event consumer goroutine: persists and logs every peer event.
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		for {
			select {
			case <-o.ctx.Done():
				return
			case ev, ok := <-eventCh:
				if !ok {
					return
				}
				// Cache actual direction (p.Inbound()) at p2p add time so that
				// connect/disconnect rows use the peer's real direction even if
				// it never completed the eth handshake (e.g. refused peers).
				o.updatePeerDir(ev)
				if err := o.logAndStorePeerEvent(runID, ev); err != nil {
					log.Warn("Failed to persist peer event", "err", err)
				}
			}
		}
	}()

	// Periodic stats logger: every 30s, report peer count and rows written
	// since start. Runs until ctx is cancelled.
	o.wg.Add(1)
	go o.statsLogger()

	// Point 3: heartbeat updates collection_runs end_ns every 60s so a crash
	// leaves a usable end time. Runs until ctx is cancelled.
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		if err := o.storage.HeartbeatCollectionRun(o.ctx, runID); err != nil && err != context.Canceled {
			log.Warn("Heartbeat stopped", "run_id", runID, "err", err)
		}
	}()

	// Wait for shutdown signal (Ctrl+C or SIGTERM)
	<-o.ctx.Done()
	exitReason := "signal"
	o.cancel()

	// Record run end time in collection_runs (ended_at / end_ns on shutdown)
	if err := o.storage.UpdateCollectionRunEndTime(runID); err != nil {
		// Non-fatal: log and continue shutting down
		log.Warn("Could not set collection run end time", "run_id", runID, "err", err)
	}

	// Persist final observed/stored/dropped counters to collection_runs at shutdown
	if err := o.storage.PersistFinalCounts(runID); err != nil {
		log.Warn("Could not persist final observation counts", "run_id", runID, "err", err)
	}

	// Wait for goroutines to finish
	o.wg.Wait()

	log.Info("Observer stopped", "run_id", runID, "exit_reason", exitReason, "uptime", time.Since(o.startTime))

	return nil
}

// setupLogLevel configures go-ethereum's root logger level from config.
// At "debug", dial and handshake errors appear; at any other value, info is
// the effective floor.
func (o *Observer) setupLogLevel() {
	level := strings.ToLower(strings.TrimSpace(o.cfg.LogLevel))
	var lvl slog.Level
	switch level {
	case "trace":
		lvl = log.LevelTrace
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	case "crit":
		lvl = log.LevelCrit
	default:
		lvl = slog.LevelInfo
	}
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, lvl, false)))
	log.Info("Log level configured", "level", level)
}

// statsLogger logs a one-line Info summary every 30s: peer count, observation
// counters (observed/stored/dropped) from storage, and uptime. Logs WARN when
// any observations have been dropped.
func (o *Observer) statsLogger() {
	defer o.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-o.ctx.Done():
			return
		case <-ticker.C:
			peerCount := o.server.PeerCount()
			observed, stored, dropped := o.storage.Stats()
			log.Info("Observer stats",
				"uptime", time.Since(o.startTime).Truncate(time.Second),
				"peers", peerCount,
				"observations_observed", observed,
				"observations_stored", stored,
				"observations_dropped", dropped,
			)
			if dropped > 0 {
				log.Warn("Observations have been dropped", "dropped", dropped)
			}
		}
	}
}

// logAndStorePeerEvent logs and persists a single peer lifecycle event.
// Direction comes from peerDir: discovery-dialed peers are outbound (true),
// inbound peers are not (false). This supersedes the old bootnode-set check
// which mislabeled discovery-dialed (dyndial) peers as inbound.
func (o *Observer) logAndStorePeerEvent(runID int64, ev *p2p.PeerEvent) error {
	o.peerDirMu.Lock()
	isOutbound, known := o.peerDir[ev.Peer]
	o.peerDirMu.Unlock()
	direction := "inbound"
	if known && isOutbound {
		direction = "outbound"
	}

	eventType := "connect"
	if ev.Type == p2p.PeerEventTypeDrop {
		eventType = "disconnect"
	}

	disconnectReason := ""
	if ev.Type == p2p.PeerEventTypeDrop && ev.Error != "" {
		disconnectReason = ev.Error
	}

	// Log every peer event so dial/handshake failures are visible.
	log.Info("Peer event",
		"run_id", runID,
		"peer", ev.Peer.String(),
		"type", eventType,
		"direction", direction,
		"remote_addr", ev.RemoteAddress,
		"local_addr", ev.LocalAddress,
		"disconnect_reason", disconnectReason,
		"protocol", ev.Protocol,
		"msg_code", ev.MsgCode,
	)

	return o.storage.StorePeerEvent(
		runID,
		o.cfg.ObserverID,
		ev.Peer.String(),
		eventType,
		direction,
		"", "", // client_name / client_version are not carried on PeerEvent
		ev.Peer.String(), // enode (peer id as string)
		ev.RemoteAddress,
		0, // eth_version unknown from PeerEvent alone
		fmt.Sprintf(`{"protocol":"%s","msg_code":%v,"disconnect_reason":"%s","local":"%s","remote":"%s"}`,
			ev.Protocol, ev.MsgCode, disconnectReason, ev.LocalAddress, ev.RemoteAddress),
	)
}

// initP2PServer sets up the p2p server with our configuration.
// It returns the number of bootnodes and static nodes parsed successfully and
// the number skipped due to parse errors, for startup logging.
func (o *Observer) initP2PServer() (bootnodesOK, bootnodesSkipped, staticNodesOK, staticNodesSkipped int, err error) {
	// Generate a private key for the server. In production, this should come
	// from secure storage or config; here it's ephemeral per process.
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Get our eth protocols: one p2p.Protocol per supported eth version (69-72).
	// The p2p server negotiates a common version with each peer, so each peer's
	// Status handshake uses the negotiated version of THAT protocol instance.
	ethProtos := o.ethHandler.ProtocolVersions()

	// Listen address from config, fallback to default if empty
	listenAddr := o.cfg.ListenAddr
	if listenAddr == "" {
		listenAddr = ":30304" // Use non-standard port to avoid conflict with stale processes
	}
	fmt.Printf("Using listen address for P2P server: %s\n", listenAddr)
	log.Info("Using listen address for P2P server", "addr", listenAddr)

	// Parse bootnodes and static nodes from config into enode.Node pointers.
	// ENR records (raw base64 enr: records) are decoded by enode.Parse;
	// legacy enode:// URLs and bare hex node IDs are parsed by enode.ParseV4.
	// A malformed entry is skipped with a clear warning and does not prevent
	// discovery from proceeding with the remaining valid entries.
	var bootstrapNodes []*enode.Node
	for _, bn := range o.cfg.Bootnodes {
		node, err := parseNode(bn)
		if err != nil {
			log.Warn("Skipping invalid bootnode", "bootnode", bn, "err", err)
			bootnodesSkipped++
			continue
		}
		bootnodesOK++
		bootstrapNodes = append(bootstrapNodes, node)
	}

	var staticNodes []*enode.Node
	for _, sn := range o.cfg.StaticNodes {
		node, err := parseNode(sn)
		if err != nil {
			log.Warn("Skipping invalid static node", "static_node", sn, "err", err)
			staticNodesSkipped++
			continue
		}
		staticNodesOK++
		staticNodes = append(staticNodes, node)
	}

	// Map the NAT specification (e.g. "extip:<public-ip>", "upnp", "stun")
	// to a p2p NAT setter. An empty spec leaves NAT unset and the node
	// advertises its listen interface (reachable locally only).
	var natInterface nat.Interface
	if o.cfg.NAT != "" {
		var err error
		natInterface, err = nat.Parse(o.cfg.NAT)
		if err != nil {
			return bootnodesOK, bootnodesSkipped, staticNodesOK, staticNodesSkipped, fmt.Errorf("failed to parse NAT spec %q: %w", o.cfg.NAT, err)
		}
		log.Info("NAT configured for P2P server", "nat", o.cfg.NAT)
	}

	// Create p2p server config
	srvConfig := p2p.Config{
		PrivateKey:     privateKey,
		Name:           "ethereum-dropper",
		ListenAddr:     listenAddr,
		Protocols:      ethProtos,
		NoDiscovery:    o.cfg.NoDiscovery, // Wire from config (not hardcoded)
		DiscoveryV4:    true,              // Enable v4 discovery (bootnode bootstrap + dial candidates) — geth mainnet default true (node/defaults.go:76)
		DiscoveryV5:    true,              // Enable v5 discovery — geth mainnet default true (node/defaults.go:77)
		NAT:            natInterface,      // Wire from cfg.NAT (nil when unset)
		BootstrapNodes: bootstrapNodes,    // Wire from cfg.Bootnodes
		StaticNodes:    staticNodes,       // Wire from cfg.StaticNodes
		MaxPeers:       o.cfg.MaxPeers,    // Apply configured max peers
	}

	// Create server
	srv := &p2p.Server{Config: srvConfig}
	o.server = srv

	log.Info("P2P effective settings",
		"listen_addr", listenAddr,
		"max_peers", o.cfg.MaxPeers,
		"no_discovery", o.cfg.NoDiscovery,
		"discovery_v4", true,
		"discovery_v5", true,
		"nat", o.cfg.NAT,
		"bootnodes_total", len(bootstrapNodes),
		"static_nodes_total", len(staticNodes),
	)

	// Track each known peer's direction (outbound=true for dialed bootnodes/static,
	// inbound=false). Direction for discovery-dialed peers is later corrected from
	// the actual *p2p.Peer via handlePeerDirection, which uses p.Inbound().
	o.peerDir = make(map[enode.ID]bool)
	for _, n := range bootstrapNodes {
		o.peerDir[n.ID()] = true // outbound (dialed)
	}
	for _, n := range staticNodes {
		o.peerDir[n.ID()] = true // outbound (dialed)
	}

	return bootnodesOK, bootnodesSkipped, staticNodesOK, staticNodesSkipped, nil
}

// parseNode parses a bootnode/static node entry into an *enode.Node.
// Raw base64 ENR records ("enr:...") are decoded by enode.Parse;
// legacy "enode://" URLs and bare hex node IDs are parsed by enode.ParseV4.
// No manual scheme concatenation is performed.
func parseNode(s string) (*enode.Node, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("empty node entry")
	}
	if strings.HasPrefix(s, "enr:") {
		return enode.Parse(enode.ValidSchemes, s)
	}
	return enode.ParseV4(s)
}

// logStatusHandshake fetches the live head block from RPC and logs the eth
// Status handshake contents (network ID, genesis hash, ForkID, latest block),
// labelled EXTERNAL per Point 1. The ForkID is computed from the live head
// fetched via RPC.
// fetchChainInfo dials the Status RPC endpoint and returns the live genesis
// block and head header. Uses a per-call timeout so a stalled RPC never hangs
// the observer.
func (o *Observer) fetchChainInfo(ctx context.Context) (*types.Block, *types.Header, error) {
	rpcEndpoint := o.cfg.StatusRPCEndpoint
	if rpcEndpoint == "" {
		return nil, nil, nil // nothing to fetch without an RPC endpoint
	}

	timeout := o.cfg.StatusRPCTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	rpcCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client, err := ethclient.Dial(rpcEndpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial RPC: %w", err)
	}
	defer client.Close()

	genesisHeader, err := client.HeaderByNumber(rpcCtx, big.NewInt(0))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get genesis block: %w", err)
	}
	head, err := client.HeaderByNumber(rpcCtx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get head block: %w", err)
	}

	return types.NewBlockWithHeader(genesisHeader), head, nil
}

// logStatusHandshake fetches the live head block via RPC and logs the eth
// Status handshake contents (network ID, genesis hash, ForkID, latest block),
// labelled EXTERNAL per Point 1, then threads the metadata into the eth
// handler. Runs once at startup.
func (o *Observer) logStatusHandshake(ctx context.Context) error {
	genesis, head, err := o.fetchChainInfo(ctx)
	if err != nil {
		return err
	}
	if genesis == nil {
		return nil // no RPC endpoint configured
	}

	forkID := forkid.NewID(params.MainnetChainConfig, genesis, head.Number.Uint64(), head.Time)

	log.Info("eth Status handshake (EXTERNAL - ForkID computed from live head via RPC)",
		"network_id", params.MainnetChainConfig.ChainID.Uint64(),
		"genesis_hash", genesis.Hash().Hex(),
		"fork_id_hash", fmt.Sprintf("%x", forkID.Hash),
		"fork_id_next", forkID.Next,
		"latest_block_number", head.Number.Uint64(),
		"latest_block_hash", head.Hash().Hex(),
		"head_timestamp", head.Time,
		"rpc_endpoint", o.cfg.StatusRPCEndpoint,
	)

	// Thread the live chain metadata into the eth handler so every peer
	// connection performs the real eth Status handshake (send + validate)
	// instead of deadlocking waiting for the peer to send Status first.
	o.ethHandler.SetChainInfo(
		params.MainnetChainConfig.ChainID.Uint64(),
		genesis,
		head.Number.Uint64(),
		head.Hash(),
		head.Time,
	)

	return nil
}

// refreshChainInfo refreshes the live head block via RPC every
// chainRefreshInterval and re-threads it into the eth handler so subsequent
// eth Status handshakes report EarliestBlock = LatestBlock = head instead of
// a stale value. Runs until ctx is cancelled (Task C).
func (o *Observer) refreshChainInfo(ctx context.Context) {
	ticker := time.NewTicker(chainRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			genesis, head, err := o.fetchChainInfo(ctx)
			if err != nil {
				log.Warn("Chain info refresh failed", "err", err)
				continue
			}
			if genesis == nil {
				continue
			}
			o.ethHandler.SetChainInfo(
				params.MainnetChainConfig.ChainID.Uint64(),
				genesis,
				head.Number.Uint64(),
				head.Hash(),
				head.Time,
			)
			log.Info("Chain info refreshed from RPC",
				"latest_block_number", head.Number.Uint64(),
				"latest_block_hash", head.Hash().Hex(),
				"head_timestamp", head.Time,
			)
		}
	}
}

// gitCommit returns the VCS revision of this build, suffixed with "+dirty" if
// the working tree has uncommitted changes, using debug.ReadBuildInfo.
func gitCommit() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			rev := s.Value
			for _, m := range bi.Settings {
				if m.Key == "vcs.modified" && m.Value == "true" {
					return rev + "+dirty"
				}
			}
			return rev
		}
	}
	return "unknown"
}

// configHash returns the sha256 hex digest of the config file at path.
func configHash(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// updatePeerDir caches p.Inbound() for ev.Peer by looking up the peer in
// o.server.Peers(). Discovery-dialed peers are outbound (!Inbound), correcting
// the bootnode-set mislabeling. Runs on every peer event while the peer is
// still present in the server peer set.
func (o *Observer) updatePeerDir(ev *p2p.PeerEvent) {
	if o.server == nil {
		return
	}
	for _, peer := range o.server.Peers() {
		if peer.ID() == ev.Peer {
			o.peerDirMu.Lock()
			o.peerDir[ev.Peer] = !peer.Inbound() // true = outbound (dialed)
			o.peerDirMu.Unlock()
			return
		}
	}
}

// rowCounts returns the number of peer_events and transaction_observations
// rows written for the given collection run. A zero count for either means no
// rows have been recorded yet (e.g. zero peers connected, no tx gossip seen).
func (o *Observer) rowCounts(runID int64) (peerEvCount int64, txObsCount int64) {
	if o.storage == nil {
		return 0, 0
	}
	db := o.storage.DB()
	if err := db.QueryRow(`SELECT COUNT(*) FROM peer_events WHERE run_id = ?`, runID).Scan(&peerEvCount); err != nil {
		log.Warn("Failed to count peer_events", "run_id", runID, "err", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM transaction_observations WHERE run_id = ?`, runID).Scan(&txObsCount); err != nil {
		log.Warn("Failed to count transaction_observations", "run_id", runID, "err", err)
	}
	return peerEvCount, txObsCount
}
