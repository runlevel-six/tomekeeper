// Package config loads and validates process configuration from the
// environment.
//
// Every setting comes from an environment variable prefixed TOME_. There is
// deliberately no config file and no command-line equivalent for these values:
// one source of truth means no precedence rules to reason about, and it is the
// shape container orchestrators want anyway.
//
// Configuration is validated once, at startup, before anything else happens. A
// process that starts with bad configuration and discovers it an hour later
// during a feed poll is worse than one that refuses to start.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"
)

// Prefix is prepended to every environment variable name.
const Prefix = "TOME_"

// Config is the validated configuration for every subcommand. Fields are
// populated only by Load, which guarantees they are usable.
type Config struct {
	// DatabaseURL is the PostgreSQL connection URL. Required.
	DatabaseURL string

	// HTTPAddr is the listen address for `tome serve`.
	HTTPAddr string

	// LogLevel is the minimum level emitted by the structured logger.
	LogLevel slog.Level

	// LogFormat is "json" or "text".
	LogFormat string

	// ShutdownTimeout bounds graceful shutdown before in-flight requests are
	// dropped.
	ShutdownTimeout time.Duration
}

// Defaults for every optional setting. Kept in one block so the reference
// documentation and the code cannot disagree by accident.
const (
	defaultHTTPAddr        = ":8080"
	defaultLogLevel        = "info"
	defaultLogFormat       = "json"
	defaultShutdownTimeout = 15 * time.Second
)

// LookupFunc matches os.LookupEnv. Taking it as a parameter keeps Load a pure
// function, which is what makes the validation table-testable.
type LookupFunc func(key string) (string, bool)

// Load reads configuration via lookup and validates it.
//
// All validation problems are reported together, not one per run. Someone
// bringing this up for the first time should learn everything that is wrong in
// a single attempt.
func Load(lookup LookupFunc) (*Config, error) {
	get := func(name, def string) string {
		if v, ok := lookup(Prefix + name); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
		return def
	}

	cfg := &Config{
		HTTPAddr:  get("HTTP_ADDR", defaultHTTPAddr),
		LogFormat: get("LOG_FORMAT", defaultLogFormat),
	}
	var problems []error

	// TOME_DATABASE_URL — required.
	raw := get("DATABASE_URL", "")
	switch {
	case raw == "":
		problems = append(problems, fmt.Errorf(
			"%sDATABASE_URL is required: a PostgreSQL connection URL, "+
				"for example postgres://tome:password@localhost:5432/tome?sslmode=disable", Prefix))
	default:
		u, err := url.Parse(raw)
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("%sDATABASE_URL is not a valid URL: %w", Prefix, err))
		case u.Scheme != "postgres" && u.Scheme != "postgresql":
			problems = append(problems, fmt.Errorf(
				"%sDATABASE_URL has scheme %q, want \"postgres\" or \"postgresql\"", Prefix, u.Scheme))
		case u.Host == "":
			problems = append(problems, fmt.Errorf("%sDATABASE_URL has no host", Prefix))
		default:
			cfg.DatabaseURL = raw
		}
	}

	// TOME_HTTP_ADDR — must be a host:port that net.Listen would accept.
	if _, port, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
		problems = append(problems, fmt.Errorf(
			"%sHTTP_ADDR %q is not a host:port address: %w", Prefix, cfg.HTTPAddr, err))
	} else if port == "" {
		problems = append(problems, fmt.Errorf(
			"%sHTTP_ADDR %q has no port", Prefix, cfg.HTTPAddr))
	}

	// TOME_LOG_LEVEL
	levelName := get("LOG_LEVEL", defaultLogLevel)
	if err := cfg.LogLevel.UnmarshalText([]byte(levelName)); err != nil {
		problems = append(problems, fmt.Errorf(
			"%sLOG_LEVEL %q is not valid, want one of: debug, info, warn, error", Prefix, levelName))
	}

	// TOME_LOG_FORMAT
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		problems = append(problems, fmt.Errorf(
			"%sLOG_FORMAT %q is not valid, want one of: json, text", Prefix, cfg.LogFormat))
	}

	// TOME_SHUTDOWN_TIMEOUT
	timeoutName := get("SHUTDOWN_TIMEOUT", defaultShutdownTimeout.String())
	d, err := time.ParseDuration(timeoutName)
	switch {
	case err != nil:
		problems = append(problems, fmt.Errorf(
			"%sSHUTDOWN_TIMEOUT %q is not a duration, for example 15s or 1m: %w", Prefix, timeoutName, err))
	case d <= 0:
		problems = append(problems, fmt.Errorf(
			"%sSHUTDOWN_TIMEOUT must be positive, got %s", Prefix, d))
	default:
		cfg.ShutdownTimeout = d
	}

	if len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return cfg, nil
}

// Error is the aggregate of every configuration problem found by Load.
type Error struct {
	Problems []error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("invalid configuration:")
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() []error { return e.Problems }

// RedactedDatabaseURL returns the database URL with any password replaced.
//
// Use this anywhere the URL might be logged. Connection strings carry
// credentials, and a log line is a wide, long-lived, frequently-shipped-
// somewhere-else surface.
func (c *Config) RedactedDatabaseURL() string {
	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		return "<unparseable>"
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

// LogValue implements slog.LogValuer so that logging a Config can never leak
// the database password, however carelessly it is called.
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("database_url", c.RedactedDatabaseURL()),
		slog.String("http_addr", c.HTTPAddr),
		slog.String("log_level", c.LogLevel.String()),
		slog.String("log_format", c.LogFormat),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
	)
}

var _ error = (*Error)(nil)
var _ slog.LogValuer = (*Config)(nil)
