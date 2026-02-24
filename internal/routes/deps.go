package routes

import (
	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"
	"github.com/scylladb/gocqlx/v3"

	"github.com/StrafeChat/equinox/internal/config"
)

type Deps struct {
	App    *fiber.App
	Config *config.Config
	Scylla gocqlx.Session
	Redis  *redis.Client
}
