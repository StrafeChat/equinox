package config

import (
	"errors"
	"strconv"

	"github.com/StrafeChat/equinox/internal/logger"
)

type Config struct {
	App      AppConfig
	HTTP     HTTPConfig
	Session  SessionConfig
	Stargate StargateConfig
	Flags    FeatureFlags
	Database DatabaseConfig
	Log      LogConfig
}

type LogConfig struct {
	Level logger.Level
}

type AppConfig struct {
	Version       string
	SnowflakeNode int64 // node ID for snowflake generator (0-1023)
}

type HTTPConfig struct {
	Port          string
	BodyLimitKB   int      // max request body size in KB (default 1024 = 1MB)
	CORSOrigins   []string // allowed origins; nil = allow all (dev)
}

type StargateConfig struct {
	Port            string   // WebSocket server port (e.g. 4001)
	Region          string   // instance region for multi-region (e.g. "us-east", "eu-west")
	AllowedOrigins  []string // allowed origins for CheckOrigin (e.g. https://web.strafe.chat)
	ReadBufferSize  int     // bytes (default 4096)
	WriteBufferSize int     // bytes (default 4096)
}

type SessionConfig struct {
	TTLSeconds int
	TokenBytes int
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
	Addr         string
	PoolSize     int    // max connections in pool (default 10)
	CacheEnabled bool   // enable Redis cache-aside for sessions, users, relationships
	CachePrefix  string // Redis key prefix (e.g. "equinox:") for cache keys
}

func Load() (*Config, error) {
	cfg := &Config{
		App: AppConfig{
			Version:       getEnvString("VERSION", "1.0.0"),
			SnowflakeNode: int64(getEnvInt("SNOWFLAKE_NODE_ID", 0)),
		},
		HTTP: HTTPConfig{
			Port:        getEnvString("PORT", "4000"),
			BodyLimitKB: getEnvInt("HTTP_BODY_LIMIT_KB", 1024),
			CORSOrigins: getEnvArray("CORS_ORIGINS", nil),
		},
		Session: SessionConfig{
			TTLSeconds: getEnvInt("SESSION_TTL_SECONDS", 86400*7), // 7 days
			TokenBytes: getEnvInt("SESSION_TOKEN_BYTES", 32),
		},
		Stargate: StargateConfig{
			Port:            getEnvString("STARGATE_PORT", "4001"),
			Region:          getEnvString("STARGATE_REGION", "default"),
			AllowedOrigins:  getEnvArray("STARGATE_ALLOWED_ORIGINS", nil),
			ReadBufferSize:  getEnvInt("STARGATE_READ_BUFFER", 4096),
			WriteBufferSize: getEnvInt("STARGATE_WRITE_BUFFER", 4096),
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
				Addr:         getEnvStringOr("REDIS_ADDR", "REDIS_ADDRS", "localhost:6379"),
				PoolSize:     getEnvInt("REDIS_POOL_SIZE", 10),
				CacheEnabled: getEnvBool("REDIS_CACHE_ENABLED", true),
				CachePrefix:  getEnvString("REDIS_CACHE_PREFIX", "equinox:"),
			},
		},
		Log: LogConfig{
			Level: logger.ParseLevel(getEnvString("LOG_LEVEL", "info")),
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
	if cfg.App.SnowflakeNode < 0 || cfg.App.SnowflakeNode > 1023 {
		return errors.New("SNOWFLAKE_NODE_ID must be 0-1023 (unique per instance)")
	}
	return nil
}
