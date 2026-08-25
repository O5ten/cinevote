// Command cinevote serves the movie voting board for a planned movie night.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/o5ten/cinevote/internal/auth"
	"github.com/o5ten/cinevote/internal/config"
	"github.com/o5ten/cinevote/internal/demo"
	"github.com/o5ten/cinevote/internal/mattermost"
	"github.com/o5ten/cinevote/internal/poster"
	"github.com/o5ten/cinevote/internal/store"
	"github.com/o5ten/cinevote/internal/web"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	demoMode := flag.Bool("demo", false,
		"start in demo mode: throwaway database seeded with accounts, films and votes")
	envFile := flag.String("env", config.DefaultEnvFile,
		"file of KEY=value settings to read if it exists; the real environment wins")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("cinevote", version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// A .env file beside the binary (or mounted into the container) is read
	// before anything looks at the environment.
	if err := config.LoadEnvFile(*envFile); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	if err := run(log, *demoMode); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, demoFlag bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if demoFlag || cfg.Demo {
		if replaced := cfg.ApplyDemoDefaults(demo.Password); replaced != "" {
			log.Info("demo mode uses a throwaway database, ignoring the configured one",
				"ignored", replaced, "using", cfg.DBPath)
		}
	}

	if dir := filepath.Dir(cfg.DBPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create data dir %s: %w", dir, err)
		}
	}
	if cfg.FreshDB {
		if err := removeDatabase(cfg.DBPath); err != nil {
			return err
		}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	st.MaxVotes = cfg.MaxVotes

	// Mattermost mode has no accounts of its own, so there is no admin account
	// to create: the admin password grants the role to whoever uses it.
	chat := mattermost.New(cfg.Mattermost.URL, cfg.Mattermost.Token)
	if cfg.UseMattermost() {
		bot, err := chat.Verify(context.Background())
		if err != nil {
			return fmt.Errorf("mattermost: %w", err)
		}
		log.Info("mattermost identifies people", "server", cfg.Mattermost.URL, "as", bot.Username)
		if cfg.AdminPassword == "" {
			generated, err := auth.Token()
			if err != nil {
				return err
			}
			cfg.AdminPassword = generated[:16]
			log.Warn("generated admin password — save it now, it is not shown again",
				"password", cfg.AdminPassword)
		}
	} else if err := bootstrapAdmin(context.Background(), st, cfg, log); err != nil {
		return err
	}

	if cfg.Demo {
		if err := seedDemo(context.Background(), st, cfg, log); err != nil {
			return err
		}
	}

	posters, err := poster.New(cfg.PosterSource, cfg.OMDbAPIKey, cfg.TMDBAPIKey)
	if err != nil {
		return err
	}
	if posters.Enabled() {
		log.Info("movie lookup enabled", "source", posters.SourceLabel())
	} else {
		log.Warn("movie lookup disabled: set OMDB_API_KEY to fetch posters automatically",
			"get_a_free_key", web.OMDbKeyURL)
	}

	srv, err := web.New(cfg, st, posters, chat, log)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go reapSessions(ctx, st, log)

	errc := make(chan error, 1)
	go func() {
		log.Info("cinevote listening",
			"version", version, "addr", cfg.Addr, "db", cfg.DBPath,
			"votes_per_user", cfg.MaxVotes, "identity", identityMode(cfg))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// identityMode names how people sign in, for the startup log.
func identityMode(cfg config.Config) string {
	if cfg.UseMattermost() {
		return "mattermost"
	}
	return "accounts"
}

// bootstrapAdmin makes sure the single admin account exists. With no password
// configured we generate one on first start and print it once — that keeps a
// fresh container usable without baking a default password into the image.
func bootstrapAdmin(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	existing, err := st.UserByUsername(ctx, cfg.AdminUsername)
	switch {
	case err == nil && cfg.AdminPassword == "":
		return nil // already set up, nothing to do
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("look up admin: %w", err)
	}

	password := cfg.AdminPassword
	generated := false
	if password == "" {
		password, err = auth.Token()
		if err != nil {
			return err
		}
		password = password[:16]
		generated = true
	} else if err := auth.ValidatePassword(password); err != nil {
		return fmt.Errorf("CINEVOTE_ADMIN_PASSWORD: %w", err)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := st.UpsertAdmin(ctx, cfg.AdminUsername, hash); err != nil {
		return fmt.Errorf("create admin: %w", err)
	}

	if generated {
		log.Warn("generated admin password — save it now, it is not shown again",
			"username", cfg.AdminUsername, "password", password)
	} else if existing != nil {
		log.Info("admin password updated from environment", "username", cfg.AdminUsername)
	} else {
		log.Info("admin account created", "username", cfg.AdminUsername)
	}
	return nil
}

// seedDemo fills an empty database with the demo movie night and prints the
// logins, so a fresh start needs no configuration at all.
func seedDemo(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	seeded, err := demo.Seed(ctx, st, cfg.AdminUsername)
	if err != nil {
		return fmt.Errorf("seed demo data: %w", err)
	}

	log.Warn("DEMO MODE — everyone shares one obvious password, do not use this for a real movie night")
	if seeded {
		log.Info("demo data created", "db", cfg.DBPath)
	} else {
		log.Info("demo data already present, left untouched", "db", cfg.DBPath)
	}
	for _, acc := range demo.Accounts(cfg.AdminUsername) {
		role := "användare"
		if acc.IsAdmin {
			role = "admin"
		}
		log.Info("demo login", "username", acc.Username, "password", demo.Password, "role", role)
	}
	return nil
}

// removeDatabase deletes a SQLite database and its write-ahead log siblings, so
// a demo restart always shows the same board.
func removeDatabase(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path+suffix, err)
		}
	}
	return nil
}

// reapSessions clears out expired logins so the table does not grow forever.
func reapSessions(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		if n, err := st.DeleteExpiredSessions(ctx); err != nil {
			if ctx.Err() == nil {
				log.Warn("session cleanup failed", "err", err)
			}
		} else if n > 0 {
			log.Info("expired sessions removed", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
