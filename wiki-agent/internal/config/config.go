// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full server configuration. See .env.example for documentation.
type Config struct {
	HTTPAddr        string
	ShutdownTimeout time.Duration

	DatabaseURL      string
	DBMaxConns       int32
	DBMinConns       int32
	DBConnectTimeout time.Duration
	MigrateOnStart   bool

	LogLevel  slog.Level
	LogFormat string

	MinConfidence     float64
	MaxKnowledgeAge   time.Duration
	SearchLimit       int
	SearchMaxDistance float64
	MaxIterations     int
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	var errs []error
	c := Config{
		HTTPAddr:          str("HTTP_ADDR", ":8080"),
		ShutdownTimeout:   dur("SHUTDOWN_TIMEOUT", 15*time.Second, &errs),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		DBMaxConns:        int32(integer("DB_MAX_CONNS", 10, &errs)),
		DBMinConns:        int32(integer("DB_MIN_CONNS", 1, &errs)),
		DBConnectTimeout:  dur("DB_CONNECT_TIMEOUT", 30*time.Second, &errs),
		MigrateOnStart:    boolean("MIGRATE_ON_START", false, &errs),
		LogFormat:         strings.ToLower(str("LOG_FORMAT", "json")),
		MinConfidence:     float("VALIDATION_MIN_CONFIDENCE", 0.7, &errs),
		MaxKnowledgeAge:   dur("VALIDATION_MAX_AGE", 30*24*time.Hour, &errs),
		SearchLimit:       integer("SEARCH_LIMIT", 5, &errs),
		SearchMaxDistance: float("SEARCH_MAX_DISTANCE", 0, &errs),
		MaxIterations:     integer("ORCHESTRATOR_MAX_ITERATIONS", 3, &errs),
	}
	if err := c.LogLevel.UnmarshalText([]byte(str("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 {
		errs = append(errs, errors.New("VALIDATION_MIN_CONFIDENCE must be within [0, 1]"))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, errors.New("LOG_FORMAT must be json or text"))
	}
	return c, errors.Join(errs...)
}

// NewLogger builds the process-wide structured logger.
func (c Config) NewLogger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

func str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func parse[T any](key string, def T, errs *[]error, fn func(string) (T, error)) T {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	out, err := fn(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return out
}

func integer(key string, def int, errs *[]error) int { return parse(key, def, errs, strconv.Atoi) }
func boolean(key string, def bool, errs *[]error) bool {
	return parse(key, def, errs, strconv.ParseBool)
}
func dur(key string, def time.Duration, errs *[]error) time.Duration {
	return parse(key, def, errs, time.ParseDuration)
}
func float(key string, def float64, errs *[]error) float64 {
	return parse(key, def, errs, func(s string) (float64, error) { return strconv.ParseFloat(s, 64) })
}
