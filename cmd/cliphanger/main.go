// ClipHanger — self-hosted still/clip extraction for Plex, Kodi and
// Jellyfin. See ../../CLAUDE.md and ../../docs/ before changing
// anything structural.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sreenathkc/cliphanger/internal/api"
	"github.com/sreenathkc/cliphanger/internal/backend"
	"github.com/sreenathkc/cliphanger/internal/queue"
	"github.com/sreenathkc/cliphanger/internal/store"
	"github.com/sreenathkc/cliphanger/internal/web"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	dataDir := envOr("DATA_DIR", "/data")
	port := envOr("PORT", "8420")
	// WORKERS is also the max-simultaneous-jobs cap: each worker only
	// ever has one job (one ffmpeg process) running at a time, so this
	// number IS the concurrency limit (confirmed 2026-08-22, per direct
	// request for a "max simultaneous jobs allowed" setting — it already
	// existed under this name, just retuned).
	workers := envIntOr("WORKERS", 3)
	// See queue.DefaultJobTimeout's own comment for why this needs to be
	// tunable — a real report of MKV-over-LAN seeks taking longer than
	// the original fixed 5 minutes.
	jobTimeout := time.Duration(envIntOr("JOB_TIMEOUT_SECONDS", int(queue.DefaultJobTimeout.Seconds()))) * time.Second

	mediaDir := filepath.Join(dataDir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		logger.Error("creating media directory", "dir", mediaDir, "error", err)
		os.Exit(1)
	}

	st, err := store.Open(filepath.Join(dataDir, "cliphanger.json"))
	if err != nil {
		logger.Error("opening store", "error", err)
		os.Exit(1)
	}

	// API_KEY env var is an OPTIONAL seed for infra-as-code setups (the
	// README's docker-compose example sets one explicitly) — see
	// Store.SeedAPIKeyIfUnset for why it only ever takes effect once,
	// on a fresh store with no key yet.
	if seed := os.Getenv("API_KEY"); seed != "" {
		if err := st.SeedAPIKeyIfUnset(seed); err != nil {
			logger.Error("seeding API key from environment", "error", err)
			os.Exit(1)
		}
	}
	apiKey, err := st.APIKey()
	if err != nil {
		logger.Error("initializing API key", "error", err)
		os.Exit(1)
	}

	// RETENTION_HOURS is an optional seed, same shape as API_KEY —
	// see Store.SeedRetentionHoursIfUnset. The default (nothing set,
	// nothing seeded) is 0 = forever; the Setup page is the ongoing way
	// to change it afterward, not this env var.
	if v := os.Getenv("RETENTION_HOURS"); v != "" {
		hours, err := strconv.Atoi(v)
		if err != nil {
			logger.Error("RETENTION_HOURS must be an integer", "value", v)
			os.Exit(1)
		}
		if err := st.SeedRetentionHoursIfUnset(hours); err != nil {
			logger.Error("seeding retention from environment", "error", err)
			os.Exit(1)
		}
	}

	logger.Info("cliphanger starting", "port", port, "dataDir", dataDir, "workers", workers, "jobTimeout", jobTimeout, "retentionHours", st.RetentionHours())
	// The web UI itself is open by default now (2026-08-23 — see
	// docs/DECISIONS.md "Web UI is open by default, not Basic-Auth-
	// walled"), so this key is only ever needed by API CLIENTS (like
	// DemoFlex), not for opening the web UI — printed in full here (not
	// just a prefix) purely so a client's first-time setup doesn't need
	// `docker exec ... cat /data/cliphanger.json` either. Find/rotate it
	// from the Setup page's API key section at any time.
	logger.Info("API key ready — clients (like DemoFlex) use this in the X-Api-Key header; find or rotate it from the web UI's Setup page too", "apiKey", apiKey)

	registry := backend.NewRegistry(
		backend.NewPlexBackend(),
		backend.NewKodiBackend(),
		backend.NewJellyfinBackend(),
	)

	q := queue.New(st, registry, mediaDir, workers, jobTimeout, logger)

	apiServer := api.New(st, q)
	webServer, err := web.New(st, q, registry, mediaDir, dataDir, logger)
	if err != nil {
		logger.Error("initializing web UI", "error", err)
		os.Exit(1)
	}

	// The web UI lives under /ui/ (StripPrefix so its own internal
	// routes stay simple), everything else — the documented,
	// externally-relied-upon client API — sits at bare paths exactly as
	// docs/API.md specifies. See web.go's own routes() comment for why
	// this split exists: GET /jobs and GET /health are both real,
	// distinct endpoints on the API AND real, distinct human pages on
	// the web UI, and the API's paths are not this project's to move.
	top := http.NewServeMux()
	top.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/setup", http.StatusFound)
	})
	top.Handle("/ui/", http.StripPrefix("/ui", webServer))
	top.Handle("/", apiServer)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go q.Run(ctx)

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           top,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "addr", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
	logger.Info("shut down cleanly")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
