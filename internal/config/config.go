package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr                 string
	MetricsAddr          string
	Bucket               string
	Region               string
	Endpoint             string
	AccessKey            string
	SecretKey            string
	UsePathStyle         bool
	Prefix               string
	AuthUser             string
	AuthPassword         string
	APIKeyEndpoint       string
	APIKeyToken          string
	ChecksumScanInterval string
	ChecksumScanPrefix   string

	// ProxyCacheTTL is how long the proxy definition list is reused before
	// being reloaded from storage.
	ProxyCacheTTL time.Duration
	// ProxyCacheStaleGrace is how long a stale definition list keeps being
	// served after a reload failure.
	ProxyCacheStaleGrace time.Duration
	// UpstreamRetryAttempts is the total number of attempts per upstream
	// request, including the first.
	UpstreamRetryAttempts int
	// UpstreamTimeout bounds a single upstream request.
	UpstreamTimeout time.Duration
	// VerifyUpstreamChecksums validates fetched artifacts against the
	// checksum upstream publishes before caching them.
	VerifyUpstreamChecksums bool
	// ScannerLeaderElection gates the checksum scanner on a shared lease so
	// only one replica scans per interval.
	ScannerLeaderElection bool
}

func Load() (Config, error) {
	cfg := Config{
		Addr:                 getenvDefault("SERVER_ADDR", ":8080"),
		MetricsAddr:          getenvDefault("METRICS_ADDR", ":9090"),
		Region:               getenvDefault("S3_REGION", "us-east-1"),
		Endpoint:             os.Getenv("S3_ENDPOINT"),
		AccessKey:            os.Getenv("S3_ACCESS_KEY"),
		SecretKey:            os.Getenv("S3_SECRET_KEY"),
		Prefix:               strings.Trim(getenvDefault("S3_PREFIX", ""), "/"),
		AuthUser:             os.Getenv("AUTH_USERNAME"),
		AuthPassword:         os.Getenv("AUTH_PASSWORD"),
		APIKeyEndpoint:       os.Getenv("AUTH_API_KEY_ENDPOINT"),
		APIKeyToken:          os.Getenv("AUTH_API_KEY_TOKEN"),
		ChecksumScanInterval: os.Getenv("CHECKSUM_SCAN_INTERVAL"),
		ChecksumScanPrefix:   strings.Trim(getenvDefault("CHECKSUM_SCAN_PREFIX", ""), "/"),
	}

	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return Config{}, fmt.Errorf("S3_BUCKET is required")
	}
	cfg.Bucket = bucket

	if v := os.Getenv("S3_USE_PATH_STYLE"); v != "" {
		usePathStyle, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("invalid S3_USE_PATH_STYLE: %w", err)
		}
		cfg.UsePathStyle = usePathStyle
	}

	var err error
	if cfg.ProxyCacheTTL, err = durationEnv("PROXY_CACHE_TTL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.ProxyCacheStaleGrace, err = durationEnv("PROXY_CACHE_STALE_GRACE", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.UpstreamTimeout, err = durationEnv("UPSTREAM_TIMEOUT", 60*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.UpstreamRetryAttempts, err = intEnv("UPSTREAM_RETRY_ATTEMPTS", 3); err != nil {
		return Config{}, err
	}
	if cfg.VerifyUpstreamChecksums, err = boolEnv("VERIFY_UPSTREAM_CHECKSUMS", true); err != nil {
		return Config{}, err
	}
	if cfg.ScannerLeaderElection, err = boolEnv("SCANNER_LEADER_ELECTION", true); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid %s: must not be negative", key)
	}
	return d, nil
}

func intEnv(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("invalid %s: must be at least 1", key)
	}
	return n, nil
}

func boolEnv(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid %s: %w", key, err)
	}
	return b, nil
}
