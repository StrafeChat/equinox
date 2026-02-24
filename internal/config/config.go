package config

import (
	"errors"
	"strconv"
)

type Config struct {
	App      AppConfig
	HTTP     HTTPConfig
	Session  SessionConfig
	Flags    FeatureFlags
	Database DatabaseConfig
}

type AppConfig struct {
	Version string
}

type HTTPConfig struct {
	Port string
}

type SessionConfig struct {
	TTLSeconds int // session expiry in seconds
	TokenBytes int // random bytes for token (e.g. 32)
}

type FeatureFlags struct {
	Captcha    bool
	Email      bool
	InviteOnly bool
}

type DatabaseConfig struct {
	Scylla ScyllaConfig
	Redis  RedisConfig
}

type ScyllaConfig struct {
	Hosts    []string
	Port     int
	Keyspace string
}

type RedisConfig struct {
	Addr string
}

func Load() (*Config, error) {
	cfg := &Config{
		App: AppConfig{
			Version: getEnvString("VERSION", "1.0.0"),
		},
		HTTP: HTTPConfig{
			Port: getEnvString("PORT", "4000"),
		},
		Session: SessionConfig{
			TTLSeconds: getEnvInt("SESSION_TTL_SECONDS", 86400*7), // 7 days
			TokenBytes: getEnvInt("SESSION_TOKEN_BYTES", 32),
		},
		Flags: FeatureFlags{
			Captcha:    getEnvBool("CAPTCHA", false),
			Email:      getEnvBool("EMAIL", false),
			InviteOnly: getEnvBool("INVITE_ONLY", false),
		},
		Database: DatabaseConfig{
			Scylla: ScyllaConfig{
				Hosts:    getEnvArray("SCYLLA_HOSTS", []string{"localhost"}),
				Port:     getEnvInt("SCYLLA_PORT", 9042),
				Keyspace: getEnvString("SCYLLA_KEYSPACE", "strafechat"),
			},
			Redis: RedisConfig{
				Addr: getEnvString("REDIS_ADDRS", "localhost"),
			},
		},
	}

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func validate(cfg *Config) error {
	if cfg.HTTP.Port == "" {
		return errors.New("PORT is required")
	}

	if _, err := strconv.Atoi(cfg.HTTP.Port); err != nil {
		return errors.New("PORT must be numeric")
	}

	return nil
}
