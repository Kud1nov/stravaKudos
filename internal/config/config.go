// Package config loads runtime configuration from environment variables.
//
// Strict env-only contract, matching systemd's EnvironmentFile= semantics:
// every knob is a single env var. Local development can put the same vars in
// a .env file at the working directory — Load() transparently reads it if
// present via godotenv, but never requires it.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all runtime knobs. See stravakudos.env.example for docs.
type Config struct {
	UserEmail    string
	UserPassword string
	ClientSecret string

	StateDir              string
	InitialTokenPath      string
	ScanInterval          time.Duration
	RosterRefreshInterval time.Duration
	MaxKudosPerPass       int
	KudosJitterMin        time.Duration
	KudosJitterMax        time.Duration

	LogLevel      string
	UserAgent     string
	StravaBaseURL string
}

// Defaults applied when the corresponding env var is absent.
const (
	DefaultStateDir              = "/var/lib/stravakudos"
	DefaultInitialTokenFile      = "initial-token"
	DefaultScanInterval          = 90 * time.Second
	DefaultRosterRefreshInterval = 24 * time.Hour
	DefaultMaxKudosPerPass       = 5
	DefaultKudosJitterMin        = 3 * time.Second
	DefaultKudosJitterMax        = 10 * time.Second
	DefaultLogLevel              = "info"
	DefaultUserAgent             = "Strava/33.0.0 (Linux; Android 8.0.0; Pixel 2 XL Build/OPD1.170816.004)"
	DefaultStravaBaseURL         = "https://m.strava.com"
)

// Load reads env vars and returns a validated Config. If a `.env` file is
// present in the working directory it is loaded first (best-effort).
func Load() (Config, error) {
	_ = godotenv.Load() // optional; never an error if absent

	cfg := Config{
		UserEmail:     os.Getenv("USER_EMAIL"),
		UserPassword:  os.Getenv("USER_PASSWORD"),
		ClientSecret:  os.Getenv("CLIENT_SECRET"),
		StateDir:      envOrDefault("STATE_DIR", DefaultStateDir),
		LogLevel:      envOrDefault("LOG_LEVEL", DefaultLogLevel),
		UserAgent:     envOrDefault("USER_AGENT", DefaultUserAgent),
		StravaBaseURL: envOrDefault("STRAVA_BASE_URL", DefaultStravaBaseURL),
	}

	cfg.InitialTokenPath = envOrDefault("INITIAL_TOKEN_PATH", cfg.StateDir+"/"+DefaultInitialTokenFile)

	var err error
	if cfg.ScanInterval, err = envDuration("SCAN_INTERVAL", DefaultScanInterval); err != nil {
		return Config{}, err
	}
	if cfg.RosterRefreshInterval, err = envDuration("ROSTER_REFRESH_INTERVAL", DefaultRosterRefreshInterval); err != nil {
		return Config{}, err
	}
	if cfg.KudosJitterMin, err = envDuration("KUDOS_JITTER_MIN", DefaultKudosJitterMin); err != nil {
		return Config{}, err
	}
	if cfg.KudosJitterMax, err = envDuration("KUDOS_JITTER_MAX", DefaultKudosJitterMax); err != nil {
		return Config{}, err
	}
	if cfg.MaxKudosPerPass, err = envInt("MAX_KUDOS_PER_PASS", DefaultMaxKudosPerPass); err != nil {
		return Config{}, err
	}

	return cfg, cfg.validate()
}

func (c Config) validate() error {
	var missing []string
	if c.UserEmail == "" {
		missing = append(missing, "USER_EMAIL")
	}
	if c.UserPassword == "" {
		missing = append(missing, "USER_PASSWORD")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "CLIENT_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env vars: %v", missing)
	}
	if c.MaxKudosPerPass < 1 {
		return errors.New("MAX_KUDOS_PER_PASS must be >= 1")
	}
	if c.KudosJitterMin > c.KudosJitterMax {
		return errors.New("KUDOS_JITTER_MIN must be <= KUDOS_JITTER_MAX")
	}
	if c.ScanInterval < time.Second {
		return errors.New("SCAN_INTERVAL must be >= 1s")
	}
	return nil
}

func envOrDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, v, err)
	}
	return d, nil
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid int %q: %w", key, v, err)
	}
	return n, nil
}
