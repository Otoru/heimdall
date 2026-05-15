package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("S3_BUCKET", "test-bucket")
	t.Setenv("S3_REGION", "")
	t.Setenv("S3_USE_PATH_STYLE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Fatalf("expected default addr :8080, got %s", cfg.Addr)
	}
	if cfg.MetricsAddr != ":9090" {
		t.Fatalf("expected default metrics addr :9090, got %s", cfg.MetricsAddr)
	}
	if cfg.Region != "us-east-1" {
		t.Fatalf("expected default region us-east-1, got %s", cfg.Region)
	}
}

func TestLoadWithOverrides(t *testing.T) {
	t.Setenv("SERVER_ADDR", ":9000")
	t.Setenv("METRICS_ADDR", ":9100")
	t.Setenv("S3_BUCKET", "bucket")
	t.Setenv("S3_REGION", "sa-east-1")
	t.Setenv("S3_PREFIX", "releases")
	t.Setenv("S3_USE_PATH_STYLE", "true")
	t.Setenv("AUTH_USERNAME", "user")
	t.Setenv("AUTH_PASSWORD", "pass")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Addr != ":9000" || cfg.MetricsAddr != ":9100" {
		t.Fatalf("unexpected addrs: %s %s", cfg.Addr, cfg.MetricsAddr)
	}
	if cfg.Region != "sa-east-1" || cfg.Prefix != "releases" {
		t.Fatalf("unexpected region/prefix: %s %s", cfg.Region, cfg.Prefix)
	}
	if !cfg.UsePathStyle {
		t.Fatalf("expected path style true")
	}
	if cfg.AuthUser != "user" || cfg.AuthPassword != "pass" {
		t.Fatalf("unexpected auth values")
	}

	// cleanup env overrides
	os.Unsetenv("S3_USE_PATH_STYLE")
}

func TestLoadMissingBucket(t *testing.T) {
	t.Setenv("S3_BUCKET", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestLoadInvalidPathStyle(t *testing.T) {
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("S3_USE_PATH_STYLE", "notabool")
	defer t.Setenv("S3_USE_PATH_STYLE", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid path style")
	}
}

func TestLoadAPIKeyEndpoint(t *testing.T) {
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("AUTH_API_KEY_ENDPOINT", "https://auth.example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKeyEndpoint != "https://auth.example.com" {
		t.Fatalf("expected APIKeyEndpoint, got %q", cfg.APIKeyEndpoint)
	}
}

func TestLoadChecksumScanConfig(t *testing.T) {
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("CHECKSUM_SCAN_INTERVAL", "15m")
	t.Setenv("CHECKSUM_SCAN_PREFIX", "/snapshots/")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ChecksumScanInterval != "15m" {
		t.Fatalf("expected 15m, got %q", cfg.ChecksumScanInterval)
	}
	if cfg.ChecksumScanPrefix != "snapshots" {
		t.Fatalf("expected trimmed prefix, got %q", cfg.ChecksumScanPrefix)
	}
}

func TestLoadPrefixTrimmed(t *testing.T) {
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("S3_PREFIX", "/my/prefix/")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Prefix != "my/prefix" {
		t.Fatalf("expected trimmed prefix, got %q", cfg.Prefix)
	}
}
