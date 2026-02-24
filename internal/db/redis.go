package db

import (
	"github.com/StrafeChat/equinox/internal/config"
	"github.com/redis/go-redis/v9"
)

func NewRedis(conf config.RedisConfig) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: conf.Addr,
	})
}
