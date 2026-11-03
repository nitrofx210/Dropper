package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ethereum/go-ethereum/log"
	"gopkg.in/urfave/cli.v1"

	"github.com/kingd/ethereum-dropper-lab/internal/config"
	"github.com/kingd/ethereum-dropper-lab/internal/observer"
)

func main() {
	app := cli.NewApp()
	app.Name = "ethereum-dropper-observer"
	app.Usage = "Ethereum P2P observer for transaction gossip"
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
	}
	app.Action = func(c *cli.Context) error {
		configFile := c.String("config")
		dataLabel := c.String("label")
		mode := c.String("mode")

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

		// Set up logging
		h := log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, false)
		log.SetDefault(log.NewLogger(h))

		// Create observer
		obs, err := observer.New(cfg, configFile)
		if err != nil {
			return fmt.Errorf("failed to create observer: %w", err)
		}

		// Set up signal handling for graceful shutdown (Ctrl+C and SIGTERM)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// Start observer
		if err := obs.Start(ctx); err != nil {
			return fmt.Errorf("observer error: %w", err)
		}

		return nil
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
