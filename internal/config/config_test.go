package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("S3_BUCKET", "test-bucket")
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
