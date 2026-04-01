package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/docker"
	"github.com/comma-compliance/arc-relay/internal/middleware"
	"github.com/comma-compliance/arc-relay/internal/oauth"
	"github.com/comma-compliance/arc-relay/internal/proxy"
	"github.com/comma-compliance/arc-relay/internal/server"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/migrations"
)

func main() {
	configPath := flag.String("config", "", "path to config file (TOML)")
	flag.Parse()

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize Sentry error tracking
	if cfg.SentryDSN != "" {
		if err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.SentryDSN,
			EnableTracing:    false,
			AttachStacktrace: true,
		}); err != nil {
			log.Printf("Warning: Sentry init failed: %v", err)
		} else {
			log.Println("Sentry error tracking enabled")
			defer sentry.Flush(2 * time.Second)
		}
	}

	// Open database with embedded migrations
	db, err := store.Open(cfg.Database.Path, migrations.FS)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Initialize stores
	crypto := store.NewConfigEncryptor(cfg.Encryption.Key)
	serverStore := store.NewServerStore(db, crypto)
	userStore := store.NewUserStore(db)
	accessStore := store.NewAccessStore(db)
	profileStore := store.NewProfileStore(db)
	requestLogStore := store.NewRequestLogStore(db)
	sessionStore := store.NewSessionStore(db)

	// Ensure default admin user exists
	adminPw := cfg.Auth.AdminPassword
	if adminPw == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			log.Fatalf("Failed to generate random admin password: %v", err)
		}
		adminPw = hex.EncodeToString(b)
		log.Println("========================================")
		log.Println("WARNING: No admin password configured!")
		log.Printf("Generated random admin password: %s", adminPw)
		log.Println("Set ARC_RELAY_ADMIN_PASSWORD to use a fixed password.")
		log.Println("========================================")
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

	// Initialize middleware
	middlewareStore := store.NewMiddlewareStore(db)
	archiveQueueStore := store.NewArchiveQueueStore(db)
	archiveEventLogger := func(evt *store.MiddlewareEvent) {
		if err := middlewareStore.LogEvent(evt); err != nil {
			log.Printf("archive dispatcher: failed to log event: %v", err)
		}
	}
	archiveDispatcher := middleware.NewArchiveDispatcher(archiveQueueStore, archiveEventLogger)
	archiveDispatcher.Start()
	mwRegistry := middleware.NewRegistry(middlewareStore, archiveDispatcher)

	// Initialize proxy manager
	proxyMgr := proxy.NewManager(serverStore, dockerMgr, oauthMgr, accessStore)

	// Auto-start all configured servers
	go func() {
		servers, err := serverStore.List()
		if err != nil {
			log.Printf("Warning: failed to list servers for auto-start: %v", err)
			return
		}
		ctx := context.Background()
		for _, s := range servers {
			if err := proxyMgr.StartServer(ctx, s); err != nil {
				log.Printf("Auto-start failed for %s: %v", s.Name, err)
			} else {
				log.Printf("Auto-started server: %s", s.Name)
			}
		}
	}()

	// Initialize invite store
	inviteStore := store.NewInviteStore(db)

	// Initialize OAuth token store (for Claude Desktop and other OAuth clients)
	oauthTokenStore := store.NewOAuthTokenStore(db)

	// Start health monitor
	healthMon := proxy.NewHealthMonitor(proxyMgr, serverStore, 30*time.Second)
	healthMon.Start()

	// Start periodic database backup (every 6 hours, keeps 2 copies)
	db.StartBackup(6 * time.Hour)

	// Periodic cleanup of expired OAuth tokens and refresh tokens
	oauthRefreshStore := store.NewOAuthRefreshTokenStore(db)
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			oauthTokenStore.Cleanup()
			oauthRefreshStore.Cleanup()
		}
	}()

	// Start HTTP server
	srv := server.New(cfg, serverStore, userStore, proxyMgr, oauthMgr, accessStore, profileStore, requestLogStore, sessionStore, middlewareStore, mwRegistry, healthMon, inviteStore, oauthTokenStore)

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Shutting down...")
		healthMon.Stop()
		archiveDispatcher.Stop()
		db.StopBackup()
		proxyMgr.StopAll(ctx)
		if dockerMgr != nil {
			dockerMgr.Close()
		}
		// Close DB explicitly before exiting so WAL is checkpointed cleanly.
		if err := db.Close(); err != nil {
			log.Printf("Warning: error closing database: %v", err)
		}
		os.Exit(0)
	}()

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
