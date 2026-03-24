package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"warreth.dev/gphotos2immich/pkg/app"
	"warreth.dev/gphotos2immich/pkg/config"
)

func main() {
	fmt.Println(">> gPhotos2Immich <<")

	cfg, err := config.ReadConfig("config.json")
	if err != nil {
		fmt.Printf("Warning: %v\n", err)
		if os.Getenv("IMMICH_API_KEY") == "" {
			fmt.Println("Please provide config.json or environment variables.")
			os.Exit(1)
		}
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid config: %v\n", err)
		os.Exit(1)
	}

	// Context with graceful shutdown on SIGINT/SIGTERM
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Printf("\nReceived %s, shutting down gracefully...\n", sig)
		cancel()
	}()

	application, err := app.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing app: %v\n", err)
		os.Exit(1)
	}

	if err := application.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
