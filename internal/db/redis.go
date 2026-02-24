package db

import (
	"strings"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/redis/go-redis/v9"
)

func NewRedis(conf config.RedisConfig) *redis.Client {
	addr := conf.Addr
	if addr != "" && !strings.Contains(addr, ":") {
		addr += ":6379"
	}
	opts := &redis.Options{
		Addr: addr,
	}
	if conf.PoolSize > 0 {
		opts.PoolSize = conf.PoolSize
	}
	return redis.NewClient(opts)
}
