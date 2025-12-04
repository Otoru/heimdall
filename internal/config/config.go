package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Addr         string
	MetricsAddr  string
	Bucket       string
	Region       string
	Endpoint     string
	AccessKey    string
	SecretKey    string
	UsePathStyle bool
	Prefix       string
	AuthUser     string
	AuthPassword string
}

func Load() (Config, error) {
	cfg := Config{
		Addr:         getenvDefault("SERVER_ADDR", ":8080"),
		MetricsAddr:  getenvDefault("METRICS_ADDR", ":9090"),
		Region:       getenvDefault("S3_REGION", "us-east-1"),
		Endpoint:     os.Getenv("S3_ENDPOINT"),
		AccessKey:    os.Getenv("S3_ACCESS_KEY"),
		SecretKey:    os.Getenv("S3_SECRET_KEY"),
		Prefix:       strings.Trim(getenvDefault("S3_PREFIX", ""), "/"),
		AuthUser:     os.Getenv("AUTH_USERNAME"),
		AuthPassword: os.Getenv("AUTH_PASSWORD"),
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

	return cfg, nil
}

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
