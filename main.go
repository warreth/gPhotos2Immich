package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"warreth.dev/gphotos2immich/pkg/app"
	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/web"
)

func main() {
	// Redirect stdout and stderr to our log buffer
	r, w, _ := os.Pipe()
	os.Stdout = w
	os.Stderr = w
	log.SetOutput(w)
	
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				web.GlobalLogBuffer.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()

	log.Println(">> gPhotos2Immich <<")

	disableWebUI := os.Getenv("DISABLE_WEBUI") == "true"
	port := 8080
	if portStr := os.Getenv("PORT"); portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil {
			port = p
		}
	}

	reloadCh := make(chan struct{}, 1)

	var (
		currentAppMu sync.RWMutex
		currentApp   *app.App
	)

	if !disableWebUI {
		// Start web UI in the background
		webUI := web.NewServer("config.json")
		webUI.OnConfigChange = func() {
			log.Println("Configuration updated via web UI.")
			select {
			case reloadCh <- struct{}{}:
			default:
			}
		}
		webUI.OnSyncTrigger = func() {
			currentAppMu.RLock()
			appl := currentApp
			currentAppMu.RUnlock()

			if appl != nil {
				log.Println("Manual sync requested via API/Web UI.")
				appl.TriggerSync()
			} else {
				log.Println("Manual sync requested but application is not initialized yet.")
			}
		}

		go func() {
			if err := webUI.Start(port); err != nil {
				log.Printf("Web server error: %v\n", err)
			}
		}()

		// Wait briefly to ensure UI server starts outputting its message
		time.Sleep(100 * time.Millisecond)
	}

	// Context with graceful shutdown on SIGINT/SIGTERM
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("\nReceived %s, shutting down gracefully...\n", sig)
		cancelRoot()
	}()

	for {
		cfg, err := config.ReadConfig("config.json")
		if err != nil || cfg.Validate() != nil {
			if err != nil {
				log.Printf("Warning: %v\n", err)
			} else {
				log.Printf("Invalid config: %v\n", cfg.Validate())
			}
			if !disableWebUI {
				log.Printf("Please verify configuration via the web UI at http://localhost:%d\n", port)
				log.Println("Waiting for configuration update...")

				select {
				case <-reloadCh:
					continue
				case <-rootCtx.Done():
					return
				}
			} else {
				log.Fatalf("Invalid configuration and Web UI is disabled. Exiting.")
			}
		}

		application, err := app.New(cfg)
		if err != nil {
			log.Printf("Error initializing app: %v\n", err)
			if !disableWebUI {
				log.Println("Waiting for configuration update...")
				select {
				case <-reloadCh:
					continue
				case <-rootCtx.Done():
					return
				}
			} else {
				log.Fatalf("Failed to initialize and Web UI is disabled. Exiting.")
			}
		}

		currentAppMu.Lock()
		currentApp = application
		currentAppMu.Unlock()

		appCtx, cancelApp := context.WithCancel(rootCtx)

		// Run app in a goroutine
		appErrCh := make(chan error, 1)
		go func() {
			defer func() {
				currentAppMu.Lock()
				if currentApp == application {
					currentApp = nil
				}
				currentAppMu.Unlock()
			}()
			appErrCh <- application.Run(appCtx)
		}()

		select {
		case <-reloadCh:
			log.Println("Reloading application...")
			cancelApp()
			<-appErrCh // Wait for current instance to shut down gracefully
		case err := <-appErrCh:
			if err != nil {
				log.Printf("Sync error: %v\n", err)
				if !disableWebUI {
					log.Println("Waiting for configuration update...")
					cancelApp()
					select {
					case <-reloadCh:
						continue
					case <-rootCtx.Done():
						return
					}
				} else {
					log.Fatalf("Fatal sync error with Web UI disabled. Exiting.")
				}
			} else {
				log.Println("Sync completed.")
				// Wait for next reload or context done
				select {
				case <-reloadCh:
				case <-rootCtx.Done():
					return
				}
			}
		case <-rootCtx.Done():
			cancelApp()
			<-appErrCh
			return
		}
	}
}
