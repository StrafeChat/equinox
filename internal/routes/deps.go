package routes

import (
	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"
	"github.com/scylladb/gocqlx/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/federation"
	"github.com/StrafeChat/equinox/internal/mail"
	"github.com/StrafeChat/equinox/internal/modules/media"
)

type Deps struct {
	App    *fiber.App
	Config *config.Config
	Scylla gocqlx.Session
	Redis  *redis.Client
	// Federation is nil unless FEDERATION_DOMAIN is configured. Route setups that own
	// federating services wire it in with their SetFederator/SetRouter hooks.
	Federation *federation.Service
	// Mailer is nil unless SMTP_HOST is configured. Auth hands it to its service; with it
	// nil, verification and password reset report "email is not configured".
	Mailer mail.Mailer
	// Media is the image proxy embeds' pictures are served through; nil when MEDIA_PROXY is
	// off or its key could not be loaded. Every messages service gets it (withMedia), so the
	// embeds it serializes carry proxy URLs.
	Media *media.Service
}
