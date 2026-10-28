// Package config loads service configuration from environment variables.
//
// Every service reads the same small set of variables; service-specific
// settings are read with the exported helpers (String, Int, Duration, ...).
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds settings shared by all OrderFlow services.
type Config struct {
	Service      string
	HTTPAddr     string
	DatabaseURL  string
	KafkaBrokers []string
	LogLevel     string
	Env          string
}

// Load reads common configuration. defaultAddr is the HTTP listen address
// used when HTTP_ADDR is not set.
func Load(service, defaultAddr string) Config {
	return Config{
		Service:      service,
		HTTPAddr:     String("HTTP_ADDR", defaultAddr),
		DatabaseURL:  String("DATABASE_URL", "postgres://orderflow:orderflow@localhost:5432/orderflow?sslmode=disable"),
		KafkaBrokers: List("KAFKA_BROKERS", []string{"localhost:29092"}),
		LogLevel:     String("LOG_LEVEL", "info"),
		Env:          String("APP_ENV", "dev"),
	}
}

// String returns the env var or def when unset/empty.
func String(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// List returns a comma-separated env var as a slice.
func List(key string, def []string) []string {
	v := String(key, "")
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Int returns an integer env var or def when unset or invalid.
func Int(key string, def int) int {
	if n, err := strconv.Atoi(String(key, "")); err == nil {
		return n
	}
	return def
}

// Int64 returns an int64 env var or def when unset or invalid.
func Int64(key string, def int64) int64 {
	if n, err := strconv.ParseInt(String(key, ""), 10, 64); err == nil {
		return n
	}
	return def
}

// Float returns a float env var or def when unset or invalid.
func Float(key string, def float64) float64 {
	if n, err := strconv.ParseFloat(String(key, ""), 64); err == nil {
		return n
	}
	return def
}

// Bool returns a boolean env var or def when unset or invalid.
func Bool(key string, def bool) bool {
	if b, err := strconv.ParseBool(String(key, "")); err == nil {
		return b
	}
	return def
}

// Duration returns a time.Duration env var (e.g. "500ms") or def.
func Duration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(String(key, "")); err == nil {
		return d
	}
	return def
}
