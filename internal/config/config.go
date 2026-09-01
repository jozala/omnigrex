package config

import (
	"fmt"
	"time"
)

const (
	defaultHTTPAddr        = ":8080"
	defaultShutdownTimeout = 10 * time.Second
)

type Config struct {
	HTTPAddr        string
	ShutdownTimeout time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	config := Config{
		HTTPAddr:        valueOrDefault(getenv("OMNIGREX_HTTP_ADDR"), defaultHTTPAddr),
		ShutdownTimeout: defaultShutdownTimeout,
	}

	if value := getenv("OMNIGREX_SHUTDOWN_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_SHUTDOWN_TIMEOUT: %w", err)
		}
		if timeout <= 0 {
			return Config{}, fmt.Errorf("OMNIGREX_SHUTDOWN_TIMEOUT must be positive")
		}
		config.ShutdownTimeout = timeout
	}

	return config, nil
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
