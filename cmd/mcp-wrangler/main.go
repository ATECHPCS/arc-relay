package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JeremiahChurch/mcp-wrangler/internal/config"
	"github.com/JeremiahChurch/mcp-wrangler/internal/docker"
	"github.com/JeremiahChurch/mcp-wrangler/internal/oauth"
	"github.com/JeremiahChurch/mcp-wrangler/internal/proxy"
	"github.com/JeremiahChurch/mcp-wrangler/internal/server"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
	"github.com/JeremiahChurch/mcp-wrangler/migrations"
)

func main() {
	configPath := flag.String("config", "", "path to config file (TOML)")
	flag.Parse()

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Open database with embedded migrations
	db, err := store.Open(cfg.Database.Path, migrations.FS)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Initialize stores
	serverStore := store.NewServerStore(db)
	userStore := store.NewUserStore(db)
	accessStore := store.NewAccessStore(db)

	// Ensure default admin user exists
	adminPw := cfg.Auth.AdminPassword
	if adminPw == "" {
		adminPw = "changeme"
	}
	if err := userStore.EnsureAdmin(adminPw); err != nil {
		log.Fatalf("Failed to ensure admin user: %v", err)
	}

	// Initialize Docker manager
	dockerMgr, err := docker.NewManager(cfg.Docker.Socket, cfg.Docker.Network)
	if err != nil {
		log.Printf("Warning: Docker not available: %v", err)
		log.Printf("Managed (stdio/http) servers will not work. Remote servers are still available.")
		dockerMgr = nil
	}

	// Initialize OAuth manager
	oauthMgr := oauth.NewManager(serverStore, cfg.PublicBaseURL())

	// Initialize proxy manager
	proxyMgr := proxy.NewManager(serverStore, dockerMgr, oauthMgr, accessStore)

	// Start health monitor
	healthMon := proxy.NewHealthMonitor(proxyMgr, serverStore, 30*time.Second)
	healthMon.Start()

	// Start HTTP server
	srv := server.New(cfg, serverStore, userStore, proxyMgr, oauthMgr, accessStore)

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Shutting down...")
		healthMon.Stop()
		proxyMgr.StopAll(ctx)
		if dockerMgr != nil {
			dockerMgr.Close()
		}
		os.Exit(0)
	}()

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
