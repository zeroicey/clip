// Package config for the clip service.
//
// It is separate from the teleport service's config package on purpose: the two
// services have different risk profiles (one is authenticated and AI-facing, the
// other is anonymous and public), and a shared struct would quietly hand each
// service knobs that only make sense for the other. What they do share is the
// response envelope and the client-IP rules, which live in internal/clientip.
package clip

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the clip service's fully-resolved configuration.
type Config struct {
	// Addr is the listen address. Production binds loopback only: the public
	// path in is frp (hpcore dials out to hcyj), so nothing should be able to
	// reach this port except the local frpc.
	Addr string

	// PublicBaseURL is the public ORIGIN, e.g. https://clip.hcyj.xyz. It is the
	// origin only because this service owns the *root* of its host: the suffix
	// is the first path segment, so there is no prefix to append.
	PublicBaseURL string

	// DatabaseURL is a libpq/pgx connection string for the relay database.
	DatabaseURL string
	// DBMaxConns bounds the pool. This service is tiny and shares a Postgres
	// instance with unrelated applications, so the pool must stay a polite
	// neighbour rather than grabbing the default 4×(cores) — 64 connections on a
	// 16-core box would be more than this service will ever use.
	DBMaxConns int32

	// Environment is echoed by /api/health.
	Environment string

	// DefaultTTL is how long a new clip lives when the creator does not say.
	// Zero or negative is NOT allowed: "never expires" must be an explicit,
	// human-made choice (the same rule teleport applies to credentials).
	DefaultTTL time.Duration

	// Body and asset limits.
	MaxBodyBytes  int64
	MaxAssetBytes int64
	MaxAssets     int

	// WritePerHour is the per-source-IP write budget.
	WritePerHour int
	// RequestPerMinute is the per-source-IP budget for EVERY request, reads
	// included. It is the anti-flood layer: the write budget is much tighter but
	// covers only writes, and before this existed a single address could drive the
	// read paths (and the database behind them) without limit. A non-positive
	// value disables it.
	RequestPerMinute int
	// BanAfter is how many refusals (429s) inside BanWindow turn into a temporary
	// block of the source address. A non-positive value disables banning.
	BanAfter int
	// BanWindow is how long refusals accumulate before they are forgotten.
	BanWindow time.Duration
	// BanFor is the first ban's duration; each later ban doubles it.
	BanFor time.Duration
	// BanMaxFor caps the escalation, so a repeat offender is never banned forever.
	BanMaxFor time.Duration
	// MaxPollsPerIP caps concurrent long polls from one source address. A
	// non-positive value disables the cap (the hub's global cap still applies).
	MaxPollsPerIP int
	// UnlockPerHour bounds password GUESSES per source address and per suffix
	// (see Server.clipAccess). Only failures are charged, so it needs to be high
	// enough that an honest reader re-sending its password on every long poll
	// never touches it — its whole job is to make guessing expensive. The same
	// budget and the same limiter cover share secrets, keyed per share token.
	UnlockPerHour int
	// ShareCodeTTL is the lifetime of a server-generated 6-digit pickup code.
	// Short on purpose: ten to the sixth possibilities are only safe behind a
	// short window plus the per-token guess budget (see Server.shareDecision).
	// A code share is never allowed to outlive this, whatever the caller asks.
	ShareCodeTTL time.Duration
	// MaxTotalClips and MaxTotalBytes are the global ceilings. They are the
	// actual backstop: a per-IP limit is a speed bump, and an attacker with a
	// handful of addresses walks around it. When either ceiling is reached the
	// service stops accepting writes and says so — it fails closed and visibly
	// rather than silently dropping the guarantee.
	MaxTotalClips int64
	MaxTotalBytes int64

	// WritesEnabled is the global kill switch. Set false to turn the service
	// read-only without taking it down or losing content.
	WritesEnabled bool
}

// Load resolves configuration from the environment, optionally seeded from a
// .env file next to the binary (developer convenience; production uses systemd's
// EnvironmentFile).
func Load() (*Config, error) {
	loadDotEnv()

	cfg := &Config{
		Addr:          env("LISTEN_ADDR", "127.0.0.1:8788"),
		PublicBaseURL: strings.TrimRight(env("PUBLIC_BASE_URL", "http://127.0.0.1:8788"), "/"),
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		Environment:   env("ENVIRONMENT", "development"),
		MaxAssets:     4,
		MaxBodyBytes:  256 << 10, // 256 KiB of text
		MaxAssetBytes: 5 << 20,   // 5 MiB per image
		MaxTotalClips: 20_000,    // ~20k clips
		MaxTotalBytes: 1 << 30,   // 1 GiB in total
		WritePerHour:  30,
		UnlockPerHour: 20,
		// The anti-flood budget. Deliberately generous next to the write budget:
		// one open tab re-polls about 2.4 times a minute, so even a desk full of
		// tabs stays far inside 240, while a scraper walking suffixes does not.
		RequestPerMinute: 240,
		BanAfter:         5,
		BanWindow:        10 * time.Minute,
		BanFor:           15 * time.Minute,
		BanMaxFor:        24 * time.Hour,
		MaxPollsPerIP:    8,
		WritesEnabled:    true,
		DefaultTTL:       24 * time.Hour,
		DBMaxConns:       8,
		ShareCodeTTL:     30 * time.Minute,
	}

	var err error
	if cfg.DefaultTTL, err = envDuration("CLIP_DEFAULT_TTL", 24*time.Hour); err != nil {
		return nil, err
	}
	if cfg.ShareCodeTTL, err = envDuration("CLIP_SHARE_CODE_TTL", 30*time.Minute); err != nil {
		return nil, err
	}
	if cfg.MaxBodyBytes, err = envInt64("CLIP_MAX_BODY_BYTES", 256<<10); err != nil {
		return nil, err
	}
	if cfg.MaxAssetBytes, err = envInt64("CLIP_MAX_ASSET_BYTES", 5<<20); err != nil {
		return nil, err
	}
	if cfg.MaxAssets, err = envInt("CLIP_MAX_ASSETS", 4); err != nil {
		return nil, err
	}
	if cfg.WritePerHour, err = envInt("CLIP_WRITE_PER_HOUR", 30); err != nil {
		return nil, err
	}
	if cfg.RequestPerMinute, err = envInt("CLIP_REQUEST_PER_MINUTE", 240); err != nil {
		return nil, err
	}
	if cfg.BanAfter, err = envInt("CLIP_BAN_AFTER", 5); err != nil {
		return nil, err
	}
	if cfg.BanWindow, err = envDuration("CLIP_BAN_WINDOW", 10*time.Minute); err != nil {
		return nil, err
	}
	if cfg.BanFor, err = envDuration("CLIP_BAN_FOR", 15*time.Minute); err != nil {
		return nil, err
	}
	if cfg.BanMaxFor, err = envDuration("CLIP_BAN_MAX_FOR", 24*time.Hour); err != nil {
		return nil, err
	}
	if cfg.MaxPollsPerIP, err = envInt("CLIP_MAX_POLLS_PER_IP", 8); err != nil {
		return nil, err
	}
	if cfg.UnlockPerHour, err = envInt("CLIP_UNLOCK_PER_HOUR", 20); err != nil {
		return nil, err
	}
	if cfg.MaxTotalClips, err = envInt64("CLIP_MAX_TOTAL_CLIPS", 20_000); err != nil {
		return nil, err
	}
	if cfg.MaxTotalBytes, err = envInt64("CLIP_MAX_TOTAL_BYTES", 1<<30); err != nil {
		return nil, err
	}
	if cfg.DBMaxConns, err = envInt32("CLIP_DB_MAX_CONNS", 8); err != nil {
		return nil, err
	}
	if cfg.WritesEnabled, err = envBool("CLIP_WRITES_ENABLED", true); err != nil {
		return nil, err
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required (e.g. postgres://relay:<password>@127.0.0.1:5432/relay?sslmode=disable)")
	}
	if cfg.PublicBaseURL == "" {
		return nil, fmt.Errorf("PUBLIC_BASE_URL must not be empty")
	}
	if cfg.MaxAssets < 1 {
		return nil, fmt.Errorf("CLIP_MAX_ASSETS must be at least 1 (got %d)", cfg.MaxAssets)
	}
	// A zero write budget would reject every write, which reads as a broken
	// deployment rather than a strict one — the same reasoning teleport's
	// KEY_APPLY_PER_HOUR carries. Use CLIP_WRITES_ENABLED for "stop accepting
	// writes"; that switch says what it means and can be flipped back.
	if cfg.WritePerHour <= 0 {
		return nil, fmt.Errorf("CLIP_WRITE_PER_HOUR must be positive (got %d)", cfg.WritePerHour)
	}
	// Non-positive would disable throttling entirely (ratelimit treats <= 0 as
	// "unlimited"), which for a guessable secret is not a configuration a
	// deployment should be able to reach by accident.
	if cfg.UnlockPerHour <= 0 {
		return nil, fmt.Errorf("CLIP_UNLOCK_PER_HOUR must be positive (got %d)", cfg.UnlockPerHour)
	}
	// A ban list is the one defence whose misconfiguration is total: it can refuse
	// everyone. So the durations are checked as a set, and the escalation is
	// required to be bounded (Base <= Max), which is what stops a repeat offender
	// from being banned forever.
	if cfg.BanAfter > 0 {
		if cfg.BanWindow <= 0 {
			return nil, fmt.Errorf("CLIP_BAN_WINDOW must be positive when banning is on (got %s)", cfg.BanWindow)
		}
		if cfg.BanFor <= 0 {
			return nil, fmt.Errorf("CLIP_BAN_FOR must be positive when banning is on (got %s)", cfg.BanFor)
		}
		if cfg.BanMaxFor < cfg.BanFor {
			return nil, fmt.Errorf("CLIP_BAN_MAX_FOR (%s) must not be shorter than CLIP_BAN_FOR (%s)", cfg.BanMaxFor, cfg.BanFor)
		}
	}
	if cfg.DefaultTTL <= 0 {
		return nil, fmt.Errorf("CLIP_DEFAULT_TTL must be positive (got %s)", cfg.DefaultTTL)
	}
	// A code share's whole security argument rests on a short window, so a TTL at
	// or above the clip's own default would silently remove it.
	if cfg.ShareCodeTTL <= 0 {
		return nil, fmt.Errorf("CLIP_SHARE_CODE_TTL must be positive (got %s)", cfg.ShareCodeTTL)
	}
	return cfg, nil
}

// ClipURL is the public, retypeable address of a clip.
func (c *Config) ClipURL(suffix string) string {
	return c.PublicBaseURL + "/" + suffix
}

// RawURL is the plain-text view, meant for curl on the other machine.
func (c *Config) RawURL(suffix string) string {
	return c.PublicBaseURL + "/" + suffix + "/raw"
}

// ShareURL is the read-only second entrance to a clip.
func (c *Config) ShareURL(token string) string {
	return c.PublicBaseURL + "/s/" + token
}

// ShareRawURL is the share's plain-text view, for curl on the other machine.
func (c *Config) ShareRawURL(token string) string {
	return c.ShareURL(token) + "/raw"
}

// ShareAssetURL addresses one image through the share entrance.
func (c *Config) ShareAssetURL(token string, position int16) string {
	return fmt.Sprintf("%s/s/%s/raw/%d", c.PublicBaseURL, token, position)
}

// ClipPath is the origin-relative path prefix of a clip's page and raw views,
// used by the page to build asset URLs without depending on its own URL shape.
func (c *Config) ClipPath(suffix string) string { return "/" + suffix }

// SharePath is ClipPath for the share entrance.
func (c *Config) SharePath(token string) string { return "/s/" + token }

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", key, raw)
	}
	return n, nil
}

func envInt32(key string, fallback int32) (int32, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, raw)
	}
	return int32(n), nil
}

func envInt64(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", key, raw)
	}
	return n, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", key, raw)
	}
	return b, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		// Go durations have no day unit, and "24h" is what this knob is for, so
		// point at the mistake rather than only reporting it.
		return 0, fmt.Errorf("%s must be a duration like 24h or 168h, got %q", key, raw)
	}
	return d, nil
}

// loadDotEnv reads a minimal KEY=VALUE file, never overriding real environment
// variables. ENV_FILE selects a path explicitly; otherwise ./.env is used.
func loadDotEnv() {
	path := os.Getenv("ENV_FILE")
	if path == "" {
		path = ".env"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}
