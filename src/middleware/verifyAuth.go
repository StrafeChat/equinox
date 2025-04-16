package middleware

import (
	"errors"
	"fmt"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

func VerifyAuth() fiber.Handler {
	return func(c fiber.Ctx) error {
		token := c.Get("X-Session-Token")
		isBot := false
		if token == "" {
			token = c.Get("X-Bot-Token")
			isBot = true
		}

		if token == "" {
			return c.Status(401).SendString("Unauthorized")
		}

		if isBot {
			botByToken := models.BotByTokenTable.SelectBuilder().
				Columns("user_id").
				Where(qb.Eq("bot_token")).
				Limit(1)

			botByTokenQuery := botByToken.Query(*database.Session).
				BindStruct(models.BotByToken{
					Token: token,
				})

			var botByTokenData models.BotByToken
			if err := botByTokenQuery.GetRelease(&botByTokenData); err != nil {
				if errors.Is(err, gocql.ErrNotFound) {
					return c.Status(401).SendString("Unauthorized")
				}
				return c.Status(500).SendString("Internal Server Error")
			}

			botById := models.BotTable.SelectBuilder().
				Columns("user_id", "public", "description", "terms_of_service_url", "privacy_policy_url").
				Where(qb.Eq("user_id")).
				Limit(1)

			botByIdQuery := botById.Query(*database.Session).
				BindStruct(models.Bot{
					UserID: botByTokenData.UserID,
				})

			var bot models.Bot
			if err := botByIdQuery.GetRelease(&bot); err != nil {
				if errors.Is(err, gocql.ErrNotFound) {
					return c.Status(401).SendString("Unauthorized")
				}
				return c.Status(500).SendString("Internal Server Error")
			}

			userById := models.UserTable.SelectBuilder().
				Columns("id", "password", "bot").
				Where(qb.Eq("id")).
				Limit(1)

			var user models.User
			userByIdQuery := userById.Query(*database.Session).
				BindStruct(models.User{
					ID: bot.UserID,
				})

			if err := userByIdQuery.GetRelease(&user); err != nil {
				if errors.Is(err, gocql.ErrNotFound) {
					return c.Status(401).SendString("Unauthorized")
				}
				return c.Status(500).SendString("Internal Server Error")
			}

			c.Locals("user", user)
			c.Locals("bot", bot)
		} else {
			sessionByToken := models.SessionTable.SelectBuilder().
				Columns("user_id", "expires_at").
				Where(qb.Eq("session_token")).
				Limit(1)

			sessionByTokenQuery := sessionByToken.Query(*database.Session).
				BindStruct(models.Session{
					Token: token,
				})

			var session models.Session
			if err := sessionByTokenQuery.GetRelease(&session); err != nil {
				if errors.Is(err, gocql.ErrNotFound) {
					return c.Status(401).SendString("Unauthorized")
				}
				fmt.Printf("Error getting session: %v", err)
				return c.Status(500).SendString("Internal Server Error")
			}

			if session.ExpiresAt.Before(time.Now()) {
				// Delete expired session from sessions table
				deleteSession := models.SessionTable.DeleteBuilder().
					Where(qb.Eq("session_token"))

				if err := deleteSession.Query(*database.Session).
					BindStruct(models.Session{Token: token}).
					ExecRelease(); err != nil {
					fmt.Printf("Error deleting expired session: %v", err)
				}

				// Also delete from sessions_by_user table
				deleteSessionByUser := models.SessionByUserTable.DeleteBuilder().
					Where(qb.Eq("user_id"), qb.Eq("session_token"))

				if err := deleteSessionByUser.Query(*database.Session).
					BindMap(qb.M{
						"user_id":       session.UserId,
						"session_token": token,
					}).
					ExecRelease(); err != nil {
					fmt.Printf("Error deleting expired session from sessions_by_user table: %v", err)
				}

				return c.Status(401).SendString("Session expired")
			}

			userById := models.UserTable.SelectBuilder().
				Columns("id", "username", "email", "discriminator", "display_name", "about_me", "bio", "bot", "created_at", "updated_at", "avatar", "banner", "accent_color", "locale", "presence", "bots", "relationships").
				Where(qb.Eq("id")).
				Limit(1)

			var user models.User
			userByIdQuery := userById.Query(*database.Session).
				BindStruct(models.User{
					ID: fmt.Sprint(session.UserId),
				})
			if err := userByIdQuery.GetRelease(&user); err != nil {
				if errors.Is(err, gocql.ErrNotFound) {
					return c.Status(401).SendString("Unauthorized")
				}
				return c.Status(500).SendString("Internal Server Error")
			}

			if user.Bot {
				var bot models.Bot
				botById := models.BotTable.SelectBuilder().
					Columns("user_id", "public", "description", "terms_of_service_url", "privacy_policy_url").
					Where(qb.Eq("user_id")).
					Limit(1)

				botByIdQuery := botById.Query(*database.Session).
					BindStruct(models.Bot{
						UserID: user.ID,
					})
				if err := botByIdQuery.GetRelease(&bot); err != nil {
					if errors.Is(err, gocql.ErrNotFound) {
						return c.Status(401).SendString("Unauthorized")
					}
					return c.Status(500).SendString("Internal Server Error")
				}
				c.Locals("user", bot)
			} else {
				c.Locals("user", user)
			}
		}

		return c.Next()
	}
}
