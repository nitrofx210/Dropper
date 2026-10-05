package config

import (
	"encoding/json"
	"io/ioutil"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// Config holds observer configuration
type Config struct {
	ListenAddr               string        `json:"listen_addr"`
	MaxPeers                 int           `json:"max_peers"`
	Mode                     string        `json:"mode"` // passive or fetch
	DataLabel                string        `json:"data_label"`
	StoragePath              string        `json:"storage_path"`
	Bootnodes                []string      `json:"bootnodes"`
	NAT                      string        `json:"nat"`
	NoDiscovery              bool          `json:"no_discovery"`
	DialTimeout              time.Duration `json:"dial_timeout"`
	TxDisseminationEnabled   bool          `json:"tx_dissemination_enabled"`
	TxDisseminationRateLimit int           `json:"tx_dissemination_rate_limit"` // per second

	// Resolver settings (optional, used by the resolver tool)
	ResolverRPCEndpoint   string        `json:"rpc_endpoint"`
	ResolverMaxAttempts   int           `json:"max_attempts"`
	ResolverBaseDelay     time.Duration `json:"base_delay"` // in seconds
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

		// Resolver settings
		ResolverRPCEndpoint:   "", // empty means not set; resolver tool will require it to be set via config or flag
		ResolverMaxAttempts:   5,
		ResolverBaseDelay:     1 * time.Second, // 1 second base delay
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