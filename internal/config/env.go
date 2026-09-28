package config

import (
	"os"
	"strconv"
	"strings"
)

func getEnvString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvStringOr(primary, secondary, fallback string) string {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		return v
	}
	if v, ok := os.LookupEnv(secondary); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			return int(parsed)
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if parsed, err := strconv.ParseBool(v); err == nil {
			return parsed
		}
	}
	return fallback
}

// getEnvArray splits a comma-separated variable, trimming whitespace around each entry
// and dropping empty ones ("a, b," -> ["a","b"]). An unset or blank variable yields
// fallback, so CORS_ORIGINS="" behaves like "not configured" rather than one empty origin.
func getEnvArray(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
