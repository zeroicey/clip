// Command clip is Relay's network clipboard.
//
// It serves one host at its root: `https://clip.hcyj.xyz/<suffix>` is a scrap of
// text (plus a few images) that somebody on another machine can open and copy.
// There is no account anywhere in the flow — reading is anonymous by design and
// writing is authorised by a token minted when the clip was created.
//
// Usage:
//
//	clip                  start the server
//	clip migrate          apply pending migrations and exit
//	clip sweep            delete expired clips once and exit
//	clip version          print the build version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zeroicey/clip/internal/clip"
)

// version is overridable at build time with
// -ldflags "-X main.version=<git describe>".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			// `version --frontend` mirrors the teleport service's flag so the two
			// services answer the same question the same way. For clip the answer is
			// always "embedded": its page is a //go:embed'd HTML file, so there is no
			// build tag and no way to produce a binary without it.
			if len(os.Args) > 2 && os.Args[2] == "--frontend" {
				fmt.Println("embedded")
				return
			}
			fmt.Println(version)
			return
		}
	}

	cfg, err := clip.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(1)
	}
	log := newLogger(cfg.Environment)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := clip.Open(ctx, cfg)
	if err != nil {
		log.Error("open store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	// The one-shot subcommands share the open/migrate path above, so `clip
	// migrate` and a normal start can never disagree about what "up to date"
	// means.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			log.Info("migrations applied")
			return
		case "sweep":
			n, err := store.Sweep(ctx)
			if err != nil {
				log.Error("sweep", "error", err)
				os.Exit(1)
			}
			log.Info("swept expired clips", "deleted", n)
			return
		}
	}

	_, router := clip.NewServer(cfg, store, version, log)

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: router,
		// A long poll legitimately holds a request for up to 25s, so the write
		// timeout has to sit above that. The read header timeout stays tight:
		// headers arrive in one packet or the client is not coming.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Janitor: expired clips are already invisible to every read, so this is
	// about reclaiming disk and freeing the suffix again. Running it inside the
	// server (rather than a system cron entry) means there is nothing to
	// configure on the host and nothing to forget after a redeploy.
	go runJanitor(ctx, store, log)

	// A listen failure (usually "address already in use") has to reach systemd
	// as a failure. Logging it and returning normally makes the unit report
	// `status=0/SUCCESS` while the service is not running, which turns a broken
	// deployment into an invisible one.
	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "public", cfg.PublicBaseURL,
			"environment", cfg.Environment, "version", version,
			"writesEnabled", cfg.WritesEnabled)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "error", err)
			serveErr <- err
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Shutdown waits for in-flight requests; a long poll returns as soon as its
	// context is cancelled, so this does not wait out the full poll window.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "error", err)
	}

	select {
	case err := <-serveErr:
		fmt.Fprintln(os.Stderr, "serve failed:", err)
		os.Exit(1)
	default:
	}
}

// runJanitor sweeps on an interval until the context is cancelled.
func runJanitor(ctx context.Context, store *clip.Store, log *slog.Logger) {
	const interval = 5 * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			n, err := store.Sweep(sweepCtx)
			cancel()
			if err != nil {
				log.Warn("sweep expired clips", "error", err)
				continue
			}
			if n > 0 {
				log.Info("swept expired clips", "deleted", n)
			}
		}
	}
}

// newLogger builds the process logger. Production emits JSON so the lines can be
// consumed by journald tooling without a parser per format; development emits
// text because a human is reading it.
func newLogger(environment string) *slog.Logger {
	if environment == "production" {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
