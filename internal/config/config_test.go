package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadOK(t *testing.T) {
	setAll(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UserEmail != "u@example.com" {
		t.Errorf("UserEmail = %q", cfg.UserEmail)
	}
	if cfg.ScanInterval != DefaultScanInterval {
		t.Errorf("ScanInterval default = %s, got %s", DefaultScanInterval, cfg.ScanInterval)
	}
	if cfg.MaxKudosPerPass != DefaultMaxKudosPerPass {
		t.Errorf("MaxKudosPerPass default = %d, got %d", DefaultMaxKudosPerPass, cfg.MaxKudosPerPass)
	}
	if cfg.StateDir != DefaultStateDir {
		t.Errorf("StateDir default = %q, got %q", DefaultStateDir, cfg.StateDir)
	}
	if cfg.InitialTokenPath != DefaultStateDir+"/"+DefaultInitialTokenFile {
		t.Errorf("InitialTokenPath derived = %q, got %q", DefaultStateDir+"/"+DefaultInitialTokenFile, cfg.InitialTokenPath)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	cases := []struct {
		name    string
		unset   string
		wantSub string
	}{
		{"no email", "USER_EMAIL", "USER_EMAIL"},
		{"no password", "USER_PASSWORD", "USER_PASSWORD"},
		{"no client secret", "CLIENT_SECRET", "CLIENT_SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setAll(t)
			t.Setenv(tc.unset, "")

			_, err := Load()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestLoadDurationParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
		ok   bool
	}{
		{"45s", 45 * time.Second, true},
		{"2m", 2 * time.Minute, true},
		{"1h30m", 90 * time.Minute, true},
		{"forever", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			setAll(t)
			t.Setenv("SCAN_INTERVAL", tc.raw)

			cfg, err := Load()
			if tc.ok && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected err, got nil")
			}
			if tc.ok && cfg.ScanInterval != tc.want {
				t.Errorf("ScanInterval = %s, want %s", cfg.ScanInterval, tc.want)
			}
		})
	}
}

func TestLoadJitterOrdering(t *testing.T) {
	setAll(t)
	t.Setenv("KUDOS_JITTER_MIN", "10s")
	t.Setenv("KUDOS_JITTER_MAX", "3s")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "JITTER_MIN") {
		t.Fatalf("expected min>max error, got: %v", err)
	}
}

func TestLoadMinKudos(t *testing.T) {
	setAll(t)
	t.Setenv("MAX_KUDOS_PER_PASS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for MAX_KUDOS_PER_PASS=0")
	}
}

// setAll sets the three required vars + isolates this test from any
// inherited env that could taint defaults.
func setAll(t *testing.T) {
	t.Helper()
	t.Setenv("USER_EMAIL", "u@example.com")
	t.Setenv("USER_PASSWORD", "pw")
	t.Setenv("CLIENT_SECRET", "secret")
	// Unset anything that would override a default-under-test:
	for _, k := range []string{
		"STATE_DIR", "INITIAL_TOKEN_PATH", "SCAN_INTERVAL",
		"ROSTER_REFRESH_INTERVAL", "MAX_KUDOS_PER_PASS",
		"KUDOS_JITTER_MIN", "KUDOS_JITTER_MAX",
		"LOG_LEVEL", "USER_AGENT", "STRAVA_BASE_URL",
	} {
		t.Setenv(k, "")
	}
}
