package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"gopkg.in/urfave/cli.v1"

	"github.com/kingd/ethereum-dropper-lab/internal/config"
	"github.com/kingd/ethereum-dropper-lab/internal/resolver"
	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

func main() {
	app := cli.NewApp()
	app.Name = "ethereum-dropper-resolver"
	app.Usage = "Ethereum transaction inclusion resolver"
	app.Version = "0.1.0"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "config",
			Value: "",
			Usage: "path to configuration file",
		},
		cli.StringFlag{
			Name:  "label",
			Value: "MAINNET_OBSERVATION",
			Usage: "data label for provenance",
		},
		cli.StringFlag{
			Name:  "mode",
			Value: "passive",
			Usage: "collection mode: passive or fetch",
		},
		cli.StringFlag{
			Name:  "rpc-endpoint",
			Value: "",
			Usage: "JSON-RPC endpoint for eth_getTransactionByHash calls",
		},
		cli.IntFlag{
			Name:  "max-attempts",
			Value: 5,
			Usage: "maximum number of attempts to check a transaction",
		},
		cli.IntFlag{
			Name:  "base-delay",
			Value: 1,
			Usage: "base delay in seconds for exponential backoff",
		},
	}
	app.Action = func(c *cli.Context) error {
		configFile := c.String("config")
		dataLabel := c.String("label")
		mode := c.String("mode")
		rpcEndpoint := c.String("rpc-endpoint")
		maxAttempts := c.Int("max-attempts")
		baseDelay := c.Int("base-delay")

		// Load configuration
		cfg, err := config.Load(configFile)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		// Override with command line flags
		if dataLabel != "" {
			cfg.DataLabel = dataLabel
		}
		if mode != "" {
			cfg.Mode = mode
		}
		if rpcEndpoint != "" {
			cfg.ResolverRPCEndpoint = rpcEndpoint
		}
		if maxAttempts != 0 {
			cfg.ResolverMaxAttempts = maxAttempts
		}
		if baseDelay != 0 {
			cfg.ResolverBaseDelay = time.Duration(baseDelay) * time.Second
		}

		// Set up logging
		h := log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, false)
		log.SetDefault(log.NewLogger(h))

		// Initialize storage
		sto, err := storage.New(cfg.StoragePath)
		if err != nil {
			return fmt.Errorf("failed to initialize storage: %w", err)
		}
		defer sto.Close()

		// Create resolver
		res, err := resolver.NewResolver(sto, cfg.ResolverRPCEndpoint, cfg.ResolverMaxAttempts, cfg.ResolverBaseDelay)
		if err != nil {
			return fmt.Errorf("failed to create resolver: %w", err)
		}
		defer res.Close()

		// Set up signal handling for graceful shutdown
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		// Start resolver
		if err := res.Run(ctx); err != nil {
			return fmt.Errorf("resolver error: %w", err)
		}

		return nil
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
