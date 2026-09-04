package prompton

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Version is the SDK version, sent as the User-Agent and in every monitoring
// log's sdk field.
const Version = "0.1.0"

// SDKName is the name this SDK reports in the sdk field of a monitoring log.
const SDKName = "prompton-go"

// DefaultHost is the PromptOn app the SDK talks to when nothing else is
// configured. The SDK appends /api/v1 itself.
const DefaultHost = "https://app.prompton.ai"

// DefaultEnvironment is the environment every runtime call defaults to. Name
// staging explicitly.
const DefaultEnvironment = "production"

// Mode selects how much of the world the client is allowed to touch.
type Mode string

// The three modes.
const (
	// ModeLive is the normal mode: poll the snapshot, send monitoring logs.
	ModeLive Mode = "live"
	// ModeTest makes no HTTP calls at all and captures monitoring logs in
	// memory for assertions. Snapshots are injected with SetSnapshot.
	ModeTest Mode = "test"
	// ModeOffline resolves from the disk cache and the bundle only. Monitoring
	// logs are kept in memory instead of being sent.
	ModeOffline Mode = "offline"
)

// Config configures a Client. Every field has a working default, and the
// precedence is explicit option, then environment variable, then default.
type Config struct {
	// Host is the PromptOn app URL, e.g. https://app.prompton.ai. The SDK
	// appends /api/v1. Env: PTN_HOST.
	Host string

	// APIKey is a runtime key, ptn_<project_slug>_… . Env: PTN_API_KEY.
	// Without one the SDK makes no remote calls and works from disk or bundle
	// only, saying so once.
	APIKey string

	// Environment is the environment this process reads. Env: PTN_ENVIRONMENT.
	// It is also the guard on the disk cache and the bundle: a staging process
	// must not boot on a production document.
	Environment string

	// Project is the project slug, used to name the disk cache. Env:
	// PTN_PROJECT. Derived from the API key when empty.
	Project string

	// Mode defaults to ModeLive.
	Mode Mode

	// CacheTTL is how long a snapshot is served from memory before the SDK
	// refreshes it with If-None-Match. Default 10s. It is also the base of the
	// failure backoff.
	CacheTTL time.Duration

	// Timeout bounds a single HTTP request. Default 5s.
	Timeout time.Duration

	// HTTPClient replaces the default client. Its own Timeout, if set, wins.
	HTTPClient *http.Client

	// DiskCachePath is where the snapshot is mirrored. Empty means a default
	// path under the OS cache (or temp) directory named by project and
	// environment.
	DiskCachePath string

	// DisableDiskCache turns the disk tier off entirely.
	DisableDiskCache bool

	// BundlePath is a snapshot JSON file shipped inside the app, used when
	// memory and disk are both empty. Commit one at migration time so a cold
	// start with no network still resolves.
	BundlePath string

	// HashEndUser sends sha256(end_user_ref) instead of the raw reference. An
	// unkeyed hash: stable within a project, not anonymisation.
	HashEndUser bool

	// Redact runs last on every monitoring-log record, after truncation. Return
	// the record to send; returning nil drops the payload.
	Redact func(map[string]interface{}) map[string]interface{}

	// PayloadDefaults applies to a use case whose snapshot carries no policy.
	PayloadDefaults PayloadPolicy

	// LogFlushInterval, LogFlushSize and LogFlushBytes are the monitoring-log
	// flush triggers. Defaults: 2s, 100 records, 1 MB.
	LogFlushInterval time.Duration
	LogFlushSize     int
	LogFlushBytes    int

	// LogMaxBuffer bounds the queue. Above it the oldest records are dropped
	// and counted. Default 10000.
	LogMaxBuffer int

	// LogMaxAttempts bounds how often one batch is retried before it is dropped
	// and counted. Default 8.
	LogMaxAttempts int

	// ShutdownTimeout bounds the best-effort flush Close performs. Default 5s.
	ShutdownTimeout time.Duration

	// Logger receives the SDK's occasional warnings. Default: the standard
	// logger with a "prompton: " prefix.
	Logger func(format string, args ...interface{})

	// UserAgent overrides the default prompton-go/<version>.
	UserAgent string

	now func() time.Time
}

func (c *Config) withDefaults() (Config, error) {
	out := *c

	if out.Host == "" {
		out.Host = os.Getenv("PTN_HOST")
	}
	if out.Host == "" {
		out.Host = DefaultHost
	}
	out.Host = strings.TrimRight(out.Host, "/")

	if out.APIKey == "" {
		out.APIKey = os.Getenv("PTN_API_KEY")
	}
	if out.Environment == "" {
		out.Environment = os.Getenv("PTN_ENVIRONMENT")
	}
	if out.Environment == "" {
		out.Environment = DefaultEnvironment
	}
	if out.Project == "" {
		out.Project = os.Getenv("PTN_PROJECT")
	}
	if out.Project == "" {
		out.Project = projectFromAPIKey(out.APIKey)
	}

	switch out.Mode {
	case "":
		out.Mode = ModeLive
	case ModeLive, ModeTest, ModeOffline:
	default:
		return out, fmt.Errorf("prompton: unknown mode %q", out.Mode)
	}

	if out.CacheTTL <= 0 {
		out.CacheTTL = 10 * time.Second
	}
	if out.Timeout <= 0 {
		out.Timeout = 5 * time.Second
	}
	if out.LogFlushInterval <= 0 {
		out.LogFlushInterval = 2 * time.Second
	}
	if out.LogFlushSize <= 0 {
		out.LogFlushSize = 100
	}
	if out.LogFlushBytes <= 0 {
		out.LogFlushBytes = 1 << 20
	}
	if out.LogMaxBuffer <= 0 {
		out.LogMaxBuffer = 10000
	}
	if out.LogMaxAttempts <= 0 {
		out.LogMaxAttempts = 8
	}
	if out.ShutdownTimeout <= 0 {
		out.ShutdownTimeout = 5 * time.Second
	}
	if out.PayloadDefaults.Mode == "" {
		out.PayloadDefaults.Mode = PayloadFull
	}
	if out.PayloadDefaults.MaxBytes <= 0 {
		out.PayloadDefaults.MaxBytes = DefaultMaxBytes
	}
	if out.PayloadDefaults.SampleRate <= 0 {
		out.PayloadDefaults.SampleRate = 1
	}
	if out.UserAgent == "" {
		out.UserAgent = SDKName + "/" + Version
	}
	if out.Logger == nil {
		out.Logger = func(format string, args ...interface{}) {
			log.Printf("prompton: "+format, args...)
		}
	}
	if out.now == nil {
		out.now = time.Now
	}
	if out.HTTPClient == nil {
		out.HTTPClient = &http.Client{Timeout: out.Timeout}
	}
	if !out.DisableDiskCache && out.DiskCachePath == "" {
		out.DiskCachePath = defaultDiskCachePath(out.Project, out.Environment)
	}
	return out, nil
}

// baseURL is the runtime API root, host plus /api/v1.
func (c *Config) baseURL() string {
	host := strings.TrimRight(c.Host, "/")
	if strings.HasSuffix(host, "/api/v1") {
		return host
	}
	return host + "/api/v1"
}

// projectFromAPIKey reads the project slug out of a ptn_<project>_<random> key.
func projectFromAPIKey(key string) string {
	if !strings.HasPrefix(key, "ptn_") {
		return ""
	}
	rest := key[len("ptn_"):]
	i := strings.LastIndex(rest, "_")
	if i <= 0 {
		return ""
	}
	return rest[:i]
}

func defaultDiskCachePath(project, environment string) string {
	if project == "" {
		project = "default"
	}
	if environment == "" {
		environment = DefaultEnvironment
	}
	name := fmt.Sprintf("%s-%s.json", sanitizePathSegment(project), sanitizePathSegment(environment))
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "prompton", name)
	}
	return filepath.Join(os.TempDir(), "prompton", name)
}

func sanitizePathSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}
