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

func getEnvBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if parsed, err := strconv.ParseBool(v); err == nil {
			return parsed
		}
	}
	return fallback
}

func getEnvArray(key string, fallback []string) []string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.Split(v, ",")
	}
	return fallback
}
