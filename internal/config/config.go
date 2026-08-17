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
	"strconv"
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

	// Username is the single v1 user, seeded by `tome migrate`.
	Username string

	// ContactURL is embedded in the outbound User-Agent so that an operator
	// who wants this archiver to stop can find out who to ask. Optional, but
	// strongly encouraged before pointing it at anyone else's server.
	ContactURL string

	// PollMinInterval and PollMaxInterval bound how often a feed is polled.
	PollMinInterval time.Duration
	PollMaxInterval time.Duration

	// FeedFailureThreshold is the number of consecutive failures after which a
	// feed is disabled and surfaced for attention.
	FeedFailureThreshold int

	// WorkerConcurrency is how many jobs `tome worker` runs at once.
	WorkerConcurrency int
}

// Defaults for every optional setting. Kept in one block so the reference
// documentation and the code cannot disagree by accident.
const (
	defaultHTTPAddr        = ":8080"
	defaultLogLevel        = "info"
	defaultLogFormat       = "json"
	defaultShutdownTimeout = 15 * time.Second

	defaultUsername             = "tome"
	defaultPollMinInterval      = 15 * time.Minute
	defaultPollMaxInterval      = 24 * time.Hour
	defaultFeedFailureThreshold = 20
	defaultWorkerConcurrency    = 5
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
		HTTPAddr:   get("HTTP_ADDR", defaultHTTPAddr),
		LogFormat:  get("LOG_FORMAT", defaultLogFormat),
		Username:   get("USERNAME", defaultUsername),
		ContactURL: get("CONTACT_URL", ""),
	}
	var problems []error

	// Collected here so that each setting below can add a problem and move on
	// rather than returning early.
	duration := func(name string, def time.Duration) time.Duration {
		raw := get(name, def.String())
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf(
				"%s%s %q is not a duration, for example 15m or 24h: %w", Prefix, name, raw, err))
			return def
		case d <= 0:
			problems = append(problems, fmt.Errorf(
				"%s%s must be positive, got %s", Prefix, name, d))
			return def
		default:
			return d
		}
	}

	positiveInt := func(name string, def int) int {
		raw := get(name, strconv.Itoa(def))
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf(
				"%s%s %q is not a whole number", Prefix, name, raw))
			return def
		case n < 1:
			problems = append(problems, fmt.Errorf(
				"%s%s must be at least 1, got %d", Prefix, name, n))
			return def
		default:
			return n
		}
	}

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

	// TOME_CONTACT_URL — optional, but must be usable if given, since it is
	// published to every server this service contacts.
	if cfg.ContactURL != "" {
		u, err := url.Parse(cfg.ContactURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			problems = append(problems, fmt.Errorf(
				"%sCONTACT_URL %q is not an absolute URL, for example https://example.com/about",
				Prefix, cfg.ContactURL))
		}
	}

	cfg.PollMinInterval = duration("POLL_MIN_INTERVAL", defaultPollMinInterval)
	cfg.PollMaxInterval = duration("POLL_MAX_INTERVAL", defaultPollMaxInterval)
	if cfg.PollMinInterval > cfg.PollMaxInterval {
		problems = append(problems, fmt.Errorf(
			"%sPOLL_MIN_INTERVAL (%s) is longer than %sPOLL_MAX_INTERVAL (%s)",
			Prefix, cfg.PollMinInterval, Prefix, cfg.PollMaxInterval))
	}

	cfg.FeedFailureThreshold = positiveInt("FEED_FAILURE_THRESHOLD", defaultFeedFailureThreshold)
	cfg.WorkerConcurrency = positiveInt("WORKER_CONCURRENCY", defaultWorkerConcurrency)

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
		slog.String("username", c.Username),
		slog.String("contact_url", c.ContactURL),
		slog.Duration("poll_min_interval", c.PollMinInterval),
		slog.Duration("poll_max_interval", c.PollMaxInterval),
		slog.Int("feed_failure_threshold", c.FeedFailureThreshold),
		slog.Int("worker_concurrency", c.WorkerConcurrency),
	)
}

var _ error = (*Error)(nil)
var _ slog.LogValuer = (*Config)(nil)
