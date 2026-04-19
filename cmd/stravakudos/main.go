// Command stravakudos is the bot daemon.
//
// Usage:
//
//	stravakudos           // run forever (systemd Type=notify)
//	stravakudos -status   // print state.db snapshot, exit non-zero if stale
//	stravakudos -version  // print build version
//
// Env contract: see internal/config and deploy/stravakudos.env.example.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/akudinov/stravakudos/internal/config"
	"github.com/akudinov/stravakudos/internal/ratelimit"
	"github.com/akudinov/stravakudos/internal/scheduler"
	"github.com/akudinov/stravakudos/internal/store"
	"github.com/akudinov/stravakudos/internal/strava"
)

// version is populated by -ldflags at build time (see Makefile).
var version = "dev"

func main() {
	statusFlag := flag.Bool("status", false, "print state.db snapshot and exit")
	versionFlag := flag.Bool("version", false, "print build version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fatal("config: %v", err)
	}
	logger := newLogger(cfg.LogLevel)
	logger.Info("startup", "version", version, "state_dir", cfg.StateDir)

	s, err := store.Open(cfg.StateDir + "/state.db")
	if err != nil {
		fatal("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if *statusFlag {
		os.Exit(runStatus(s))
	}

	stravaClient := strava.NewClient(cfg.StravaBaseURL, cfg.UserAgent, 30*time.Second)
	adapter := &tokenStoreAdapter{s: s}
	authMgr := strava.NewAuthManager(stravaClient, adapter, cfg.ClientSecret, cfg.UserEmail, cfg.UserPassword, logger)

	// Import legacy .strava-auth-token if present (first boot migration).
	if err := authMgr.ImportInitialToken(context.Background(), cfg.InitialTokenPath); err != nil {
		logger.Warn("initial token import failed", "err", err)
		// non-fatal — Ensure will password-grant if no token lands
	}

	rl := ratelimit.NewLimiter()

	sch := scheduler.New(
		stravaClient,
		s,
		authMgr,
		rl,
		scheduler.Config{
			ScanInterval:          cfg.ScanInterval,
			RosterRefreshInterval: cfg.RosterRefreshInterval,
			MaxKudosPerPass:       cfg.MaxKudosPerPass,
			KudosJitterMin:        cfg.KudosJitterMin,
			KudosJitterMax:        cfg.KudosJitterMax,
		},
		logger,
	)
	sch.SetWatchdog(func() { _, _ = daemon.SdNotify(false, daemon.SdNotifyWatchdog) })

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if _, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		logger.Warn("sd_notify READY failed (not running under systemd?)", "err", err)
	}
	logger.Info("scheduler start", "scan_interval", cfg.ScanInterval, "max_kudos", cfg.MaxKudosPerPass)

	if err := sch.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("scheduler exited with error", "err", err)
		_, _ = daemon.SdNotify(false, daemon.SdNotifyStopping)
		os.Exit(1)
	}
	_, _ = daemon.SdNotify(false, daemon.SdNotifyStopping)
	logger.Info("shutdown complete")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// runStatus prints a concise snapshot and exits non-zero if the last
// successful kudos is older than 7 days (so an external cron/script can
// alert on it without needing journalctl).
func runStatus(s *store.Store) int {
	ctx := context.Background()
	var athletes, kudosTotal, kudosPosted int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM athletes`).Scan(&athletes)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM kudos_log`).Scan(&kudosTotal)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM kudos_log WHERE api_status = 201`).Scan(&kudosPosted)

	var last429 int64
	_ = s.DB().QueryRow(`SELECT COALESCE(MAX(ts), 0) FROM api_calls WHERE status = 429`).Scan(&last429)

	var lastPost int64
	_ = s.DB().QueryRow(`SELECT COALESCE(MAX(kudoed_at), 0) FROM kudos_log WHERE api_status = 201`).Scan(&lastPost)

	now := time.Now().Unix()
	fmt.Printf("athletes:       %d\n", athletes)
	fmt.Printf("kudos (total):  %d\n", kudosTotal)
	fmt.Printf("kudos (posted): %d\n", kudosPosted)
	if lastPost == 0 {
		fmt.Printf("last posted:    never\n")
	} else {
		fmt.Printf("last posted:    %s (%s ago)\n",
			time.Unix(lastPost, 0).Format(time.RFC3339),
			time.Duration(now-lastPost)*time.Second)
	}
	if last429 == 0 {
		fmt.Printf("last 429:       never\n")
	} else {
		fmt.Printf("last 429:       %s (%s ago)\n",
			time.Unix(last429, 0).Format(time.RFC3339),
			time.Duration(now-last429)*time.Second)
	}

	// Last 10 posted kudos
	rows, err := s.DB().QueryContext(ctx, `
		SELECT activity_id, athlete_id, kudoed_at, api_status
		FROM kudos_log
		ORDER BY kudoed_at DESC
		LIMIT 10`)
	if err == nil {
		defer func() { _ = rows.Close() }()
		fmt.Printf("\nlast kudos:\n")
		for rows.Next() {
			var actID, athID, ts int64
			var status int
			_ = rows.Scan(&actID, &athID, &ts, &status)
			fmt.Printf("  %s  activity=%d athlete=%d status=%d\n",
				time.Unix(ts, 0).Format(time.RFC3339), actID, athID, status)
		}
	}

	// Exit non-zero if no posted kudos in last 7 days
	sevenDays := int64(7 * 24 * 3600)
	if lastPost == 0 || now-lastPost > sevenDays {
		return 1
	}
	return 0
}

// tokenStoreAdapter bridges store.TokenRecord ↔ strava.StoredToken so that
// strava doesn't need to import store (which would create a cycle via
// scheduler.Store → store and strava.TokenStore → store).
type tokenStoreAdapter struct{ s *store.Store }

func (a *tokenStoreAdapter) GetToken(ctx context.Context) (*strava.StoredToken, error) {
	t, err := a.s.GetToken(ctx)
	if err != nil || t == nil {
		return nil, err
	}
	return &strava.StoredToken{
		AccessToken: t.AccessToken,
		ObtainedAt:  t.ObtainedAt,
		ExpiresAt:   t.ExpiresAt,
	}, nil
}
func (a *tokenStoreAdapter) SetToken(ctx context.Context, token string, exp time.Time) error {
	return a.s.SetToken(ctx, token, exp)
}
func (a *tokenStoreAdapter) ClearToken(ctx context.Context) error {
	return a.s.ClearToken(ctx)
}
