package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/ethereum/go-ethereum/log"
	"gopkg.in/urfave/cli.v1"

	"github.com/kingd/ethereum-dropper-lab/internal/config"
	"github.com/kingd/ethereum-dropper-lab/internal/resolver"
	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

func main() {
	app := cli.NewApp()
	app.Name = "ethereum-dropper-blockresolver"
	app.Usage = "Ethereum transaction inclusion resolver using block-based fetching"
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
			Usage: "JSON-RPC endpoint for eth_getBlockByNumber calls",
		},
		cli.IntFlag{
			Name:  "post-run-margin",
			Value: 3600, // 1 hour in seconds
			Usage: "post-run margin in seconds to wait after observation end",
		},
		cli.IntFlag{
			Name:  "batch-size",
			Value: 20,
			Usage: "number of blocks to fetch per RPC batch request",
		},
		cli.IntFlag{
			Name:  "max-concurrent-requests",
			Value: 5,
			Usage: "maximum number of concurrent block fetching requests",
		},
	}
	app.Action = func(c *cli.Context) error {
		configFile := c.String("config")
		dataLabel := c.String("label")
		mode := c.String("mode")
		rpcEndpoint := c.String("rpc-endpoint")
		postRunMargin := c.Int("post-run-margin")
		batchSize := c.Int("batch-size")
		maxConcurrent := c.Int("max-concurrent-requests")

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
		if postRunMargin != 0 {
			cfg.ResolverPostRunMargin = postRunMargin
		}
		if batchSize != 0 {
			cfg.ResolverBatchSize = batchSize
		}
		if maxConcurrent != 0 {
			cfg.ResolverMaxConcurrentRequests = maxConcurrent
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

		// Create block resolver
		res, err := resolver.NewBlockResolver(sto, cfg.ResolverRPCEndpoint, cfg.ResolverBatchSize, cfg.ResolverMaxConcurrentRequests, int64(cfg.ResolverPostRunMargin))
		if err != nil {
			return fmt.Errorf("failed to create block resolver: %w", err)
		}
		defer res.Close()

		// Set up signal handling for graceful shutdown
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		// Start resolver
		if err := res.Run(ctx); err != nil {
			return fmt.Errorf("block resolver error: %w", err)
		}

		return nil
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}