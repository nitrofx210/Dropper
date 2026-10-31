package config

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// utf8BOM is a UTF-8 byte order mark (EF BB BF).
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// stripBOM removes a leading UTF-8 BOM if present so json.Unmarshal does not
// fail on files that begin with one.
func stripBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, utf8BOM)
}

// Config holds observer configuration
type Config struct {
	ListenAddr               string        `json:"listen_addr"`
	MaxPeers                 int           `json:"max_peers"`
	Mode                     string        `json:"mode"` // passive or fetch
	DataLabel                string        `json:"data_label"`
	StoragePath              string        `json:"storage_path"`
	Bootnodes                []string      `json:"bootnodes"`
	StaticNodes              []string      `json:"static_nodes"`
	NAT                      string        `json:"nat"`
	NoDiscovery              bool          `json:"no_discovery"`
	DialTimeout              time.Duration `json:"dial_timeout"`
	TxDisseminationEnabled   bool          `json:"tx_dissemination_enabled"`
	TxDisseminationRateLimit int           `json:"tx_dissemination_rate_limit"` // per second

	// Run metadata (for collection_runs row + append-only inclusion labeling)
	ObserverID       string `json:"observer_id"`
	GethLibVersion   string `json:"geth_lib_version"`
	GoBuildVersion   string `json:"go_build_version"`
	CollectorGitCommit string `json:"collector_git_commit"`
	ConfigHash       string `json:"config_hash"`
	SchemaVersion    string `json:"schema_version"`
	Notes            string `json:"notes"`

	// Resolver settings (optional, used by the resolver tool)
	ResolverRPCEndpoint   string        `json:"rpc_endpoint"`
	ResolverMaxAttempts   int           `json:"max_attempts"`

	// RPC endpoint used to fetch the live head block for computing the eth
	// Status handshake ForkID (Point 1: labelled EXTERNAL when logged).
	StatusRPCEndpoint  string        `json:"status_rpc_endpoint"`
	StatusRPCTimeout   time.Duration `json:"status_rpc_timeout"` // default 10s

	// Log level for the observer ("trace", "debug", "info", "warn", "error", "crit").
	// Defaults to "info". At "debug", go-ethereum's root logger is set to debug so
	// dial and handshake errors appear.
	LogLevel string `json:"log_level"`
	ResolverBaseDelay     time.Duration `json:"base_delay"` // in seconds
	ResolverPostRunMargin int           `json:"post_run_margin"` // in seconds
	ResolverBatchSize     int           `json:"batch_size"`
	ResolverMaxConcurrentRequests int     `json:"max_concurrent_requests"`
}

// MainnetConfig returns the default mainnet configuration
func MainnetConfig() *Config {
	return &Config{
		ListenAddr:               ":30306", // Use non-standard port to avoid conflict with stale processes during testing
		MaxPeers:                 50,
		Mode:                     "passive",
		DataLabel:                "MAINNET_OBSERVATION",
		StoragePath:              "observer.db",
		Bootnodes:                params.MainnetBootnodes,
		NAT:                      "",
		NoDiscovery:              false,
		DialTimeout:              5 * time.Second,
		TxDisseminationEnabled:   false,
		TxDisseminationRateLimit: 10, // 10 per second by default

		// Run metadata defaults
		ObserverID:       "observer-001",
		GethLibVersion:   "go-ethereum/v1.17.5",
		GoBuildVersion:   "go1.26.5",
		CollectorGitCommit: "",
		ConfigHash:       "",
		SchemaVersion:    "1",
		Notes:            "",
		LogLevel:         "info",

		// Resolver settings
		ResolverRPCEndpoint:   "", // empty means not set; resolver tool will require it to be set via config or flag
		ResolverMaxAttempts:   5,
		ResolverBaseDelay:     1 * time.Second, // 1 second base delay
		ResolverPostRunMargin: 3600, // 1 hour in seconds
		ResolverBatchSize:     20,
		ResolverMaxConcurrentRequests: 5,
	}
}

// Load loads configuration from a file
func Load(configFile string) (*Config, error) {
	if configFile == "" {
		return MainnetConfig(), nil
	}

	data, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	// Strip a leading UTF-8 BOM so json.Unmarshal doesn't choke on BOM-ed files.
	data = stripBOM(data)

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	log.Info("Loaded config", "config", cfg)

	// Set defaults for any missing fields
	defaultCfg := MainnetConfig()
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultCfg.ListenAddr
	}
	if cfg.MaxPeers == 0 {
		cfg.MaxPeers = defaultCfg.MaxPeers
	}
	if cfg.Mode == "" {
		cfg.Mode = defaultCfg.Mode
	}
	if cfg.DataLabel == "" {
		cfg.DataLabel = defaultCfg.DataLabel
	}
	if cfg.StoragePath == "" {
		cfg.StoragePath = defaultCfg.StoragePath
	}
	if len(cfg.Bootnodes) == 0 {
		cfg.Bootnodes = defaultCfg.Bootnodes
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultCfg.DialTimeout
	}
	if cfg.TxDisseminationRateLimit == 0 {
		cfg.TxDisseminationRateLimit = defaultCfg.TxDisseminationRateLimit
	}
	if cfg.ObserverID == "" {
		cfg.ObserverID = defaultCfg.ObserverID
	}
	if cfg.GethLibVersion == "" {
		cfg.GethLibVersion = defaultCfg.GethLibVersion
	}
	if cfg.GoBuildVersion == "" {
		cfg.GoBuildVersion = defaultCfg.GoBuildVersion
	}
	if cfg.CollectorGitCommit == "" {
		cfg.CollectorGitCommit = defaultCfg.CollectorGitCommit
	}
	if cfg.ConfigHash == "" {
		cfg.ConfigHash = defaultCfg.ConfigHash
	}
	if cfg.SchemaVersion == "" {
		cfg.SchemaVersion = defaultCfg.SchemaVersion
	}
	if cfg.Notes == "" {
		cfg.Notes = defaultCfg.Notes
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = defaultCfg.LogLevel
	}
	// Note: TxDisseminationEnabled defaults to false, which is already the zero value.
	if cfg.ResolverRPCEndpoint == "" {
		cfg.ResolverRPCEndpoint = defaultCfg.ResolverRPCEndpoint
	}
	if cfg.ResolverMaxAttempts == 0 {
		cfg.ResolverMaxAttempts = defaultCfg.ResolverMaxAttempts
	}
	if cfg.ResolverBaseDelay == 0 {
		cfg.ResolverBaseDelay = defaultCfg.ResolverBaseDelay
	}

	return &cfg, nil
}