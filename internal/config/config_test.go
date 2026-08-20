package config_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/config"
)

// env builds a LookupFunc over a literal map, so each test states exactly the
// environment it runs in and nothing leaks between cases.
func env(m map[string]string) config.LookupFunc {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

const validDSN = "postgres://tome:secret@db:5432/tome?sslmode=disable"

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"TOME_DATABASE_URL": validDSN}))
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}

	if got, want := cfg.HTTPAddr, ":8080"; got != want {
		t.Errorf("HTTPAddr = %q, want %q", got, want)
	}
	if got, want := cfg.LogLevel, slog.LevelInfo; got != want {
		t.Errorf("LogLevel = %v, want %v", got, want)
	}
	if got, want := cfg.LogFormat, "json"; got != want {
		t.Errorf("LogFormat = %q, want %q", got, want)
	}
	if got, want := cfg.ShutdownTimeout, 15*time.Second; got != want {
		t.Errorf("ShutdownTimeout = %v, want %v", got, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"TOME_DATABASE_URL":     validDSN,
		"TOME_HTTP_ADDR":        "127.0.0.1:9999",
		"TOME_LOG_LEVEL":        "debug",
		"TOME_LOG_FORMAT":       "text",
		"TOME_SHUTDOWN_TIMEOUT": "45s",
	}))
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}

	if got, want := cfg.HTTPAddr, "127.0.0.1:9999"; got != want {
		t.Errorf("HTTPAddr = %q, want %q", got, want)
	}
	if got, want := cfg.LogLevel, slog.LevelDebug; got != want {
		t.Errorf("LogLevel = %v, want %v", got, want)
	}
	if got, want := cfg.LogFormat, "text"; got != want {
		t.Errorf("LogFormat = %q, want %q", got, want)
	}
	if got, want := cfg.ShutdownTimeout, 45*time.Second; got != want {
		t.Errorf("ShutdownTimeout = %v, want %v", got, want)
	}
}

// An empty or whitespace-only variable is treated as unset. Compose files and
// Kubernetes manifests produce empty strings far more often than they produce
// deliberate empties, and "" is not a valid value for any setting here.
func TestLoadEmptyValueFallsBackToDefault(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"TOME_DATABASE_URL": validDSN,
		"TOME_HTTP_ADDR":    "   ",
	}))
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if got, want := cfg.HTTPAddr, ":8080"; got != want {
		t.Errorf("HTTPAddr = %q, want %q", got, want)
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name    string
		environ map[string]string
		wantIn  string // substring the message must contain
	}{
		{
			name:    "database url missing",
			environ: map[string]string{},
			wantIn:  "TOME_DATABASE_URL is required",
		},
		{
			name:    "database url empty",
			environ: map[string]string{"TOME_DATABASE_URL": ""},
			wantIn:  "TOME_DATABASE_URL is required",
		},
		{
			name:    "database url wrong scheme",
			environ: map[string]string{"TOME_DATABASE_URL": "mysql://db:3306/tome"},
			wantIn:  `has scheme "mysql"`,
		},
		{
			name:    "database url has no host",
			environ: map[string]string{"TOME_DATABASE_URL": "postgres:///tome"},
			wantIn:  "has no host",
		},
		{
			name: "http addr has no port",
			environ: map[string]string{
				"TOME_DATABASE_URL": validDSN,
				"TOME_HTTP_ADDR":    "localhost",
			},
			wantIn: "is not a host:port address",
		},
		{
			name: "log level invalid",
			environ: map[string]string{
				"TOME_DATABASE_URL": validDSN,
				"TOME_LOG_LEVEL":    "verbose",
			},
			wantIn: "TOME_LOG_LEVEL",
		},
		{
			name: "log format invalid",
			environ: map[string]string{
				"TOME_DATABASE_URL": validDSN,
				"TOME_LOG_FORMAT":   "logfmt",
			},
			wantIn: "TOME_LOG_FORMAT",
		},
		{
			name: "shutdown timeout not a duration",
			environ: map[string]string{
				"TOME_DATABASE_URL":     validDSN,
				"TOME_SHUTDOWN_TIMEOUT": "30",
			},
			wantIn: "is not a duration",
		},
		{
			name: "shutdown timeout not positive",
			environ: map[string]string{
				"TOME_DATABASE_URL":     validDSN,
				"TOME_SHUTDOWN_TIMEOUT": "0s",
			},
			wantIn: "must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Load(env(tt.environ))
			if err == nil {
				t.Fatalf("Load() = %+v, want an error", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("Load() error = %q,\nwant it to contain %q", err, tt.wantIn)
			}
		})
	}
}

// Every problem should surface on the first run, not one per attempt.
func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	_, err := config.Load(env(map[string]string{
		"TOME_LOG_LEVEL":  "verbose",
		"TOME_LOG_FORMAT": "logfmt",
	}))
	if err == nil {
		t.Fatal("Load() = nil error, want an error")
	}

	var cfgErr *config.Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("Load() error is %T, want *config.Error", err)
	}
	if got, want := len(cfgErr.Problems), 3; got != want {
		t.Errorf("reported %d problems, want %d:\n%v", got, want, err)
	}

	msg := err.Error()
	for _, want := range []string{"TOME_DATABASE_URL", "TOME_LOG_LEVEL", "TOME_LOG_FORMAT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %s:\n%s", want, msg)
		}
	}
}

// The database password must not survive a trip through the logger. This is
// the one config test that is a security property rather than a usability one.
func TestRedactedDatabaseURL(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"TOME_DATABASE_URL": validDSN}))
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}

	redacted := cfg.RedactedDatabaseURL()
	if strings.Contains(redacted, "secret") {
		t.Errorf("RedactedDatabaseURL() = %q, still contains the password", redacted)
	}
	// The username, host, database, and options stay: they are what makes the
	// startup log useful for "is it pointed at the right database".
	if !strings.Contains(redacted, "tome:xxxxx@db:5432/tome?sslmode=disable") {
		t.Errorf("RedactedDatabaseURL() = %q, want the user, host, and options preserved", redacted)
	}

	// The same must hold for the slog.LogValuer path, which is how the URL
	// actually reaches a log line at startup.
	if strings.Contains(renderLogValue(cfg), "secret") {
		t.Error("LogValue() leaks the database password")
	}
}

func TestRedactedDatabaseURLWithoutPassword(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"TOME_DATABASE_URL": "postgres://db:5432/tome"}))
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if got, want := cfg.RedactedDatabaseURL(), "postgres://db:5432/tome"; got != want {
		t.Errorf("RedactedDatabaseURL() = %q, want %q unchanged", got, want)
	}
}

func renderLogValue(cfg *config.Config) string {
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("test", "config", cfg)
	return b.String()
}

// A database password containing a URL-special character is the first-run failure
// this project shipped instructions for, and the error it produced leaked the
// password.
//
// Both halves are asserted here. The message has to name the cause, because
// "not a valid URL" about a value nobody typed by hand sends an operator looking at
// the wrong thing entirely — and it must not echo the value, because this runs in
// every pod that cannot start and stderr is a container log. The config summary is
// careful about secrets; an error message that undid that would be the same secret
// somewhere more visible.
func TestAnUnparseableDatabaseURLNeitherEchoesNorMystifies(t *testing.T) {
	// Exactly the shape `openssl rand -base64 24` used to hand out: a slash in the
	// password, which ends the authority section.
	const password = "NITVMRxV07fvDj3qYD/6oID9EiGuabFG"
	dsn := "postgres://tome:" + password + "@postgres:5432/tome?sslmode=disable"

	_, err := config.Load(env(map[string]string{"TOME_DATABASE_URL": dsn}))
	if err == nil {
		t.Fatal("Load() accepted a DSN whose password breaks the URL")
	}
	msg := err.Error()

	// It says which characters, so the remedy is readable from the message.
	for _, want := range []string{"TOME_DATABASE_URL is not a valid URL", "percent-encoded", "/"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q:\n%s", want, msg)
		}
	}

	// And it does not carry the credential. Checked against the password and against
	// the fragment url.Parse quotes back, which is a prefix of it.
	if strings.Contains(msg, password) {
		t.Errorf("the error message contains the database password:\n%s", msg)
	}
	if strings.Contains(msg, "NITVMRxV07fvDj3qYD") {
		t.Errorf("the error message contains part of the database password:\n%s", msg)
	}
	if strings.Contains(msg, dsn) {
		t.Errorf("the error message contains the whole DSN:\n%s", msg)
	}
}
