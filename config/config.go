package config

import (
	"os"
	"strconv"
)

type Config struct {
	Port       string
	Version    string
	Captcha    bool
	Email      bool
	InviteOnly bool
}

func Load() (*Config, error) {
	cfg := &Config{
		Port:       getEnvString("PORT", "4000"),
		Version:    getEnvString("VERSION", "1.0.0"),
		Captcha:    getEnvBool("CAPTCHA", false),
		Email:      getEnvBool("EMAIL", false),
		InviteOnly: getEnvBool("INVITE_ONLY", false),
	}

	// implement checking and error for required config variables

	return cfg, nil
}

func getEnvString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return fallback
}
