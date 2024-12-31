package helpers

import (
	"log"

	"github.com/StrafeChat/equinox/src/database/models"
)

func ClientUserResponseFormat(user models.User, relationships []models.Relationship) map[string]interface{} {
	log.Printf("Formatting response for user: %+v", user)
	log.Printf("With relationships: %+v", relationships)
	
	clientUser := map[string]interface{}{
		"id":            user.ID,
		"username":      user.Username,
		"email":         user.Email,
		"discriminator": user.Discriminator,
		"display_name":  user.DisplayName,
		"avatar":        user.Avatar,
		"banner":        user.Banner,
		"bot":           user.Bot,
		"bots":          user.Bots,
		"system":        user.System,
		"bio":           user.Bio,
		"flags":         user.Flags,
		"about_me":      user.AboutMe,
		"accent_color":  user.AccentColor,
		"locale":        user.Locale,
		"presence":      user.Presence,
		"created_at":    user.CreatedAt,
		"updated_at":    user.UpdatedAt,
	}
	
	response := map[string]interface{}{
		"client_user":      clientUser,
		"relationships":    relationships,
	}
	
	log.Printf("Final response: %+v", response)
	return response
}

func GetUserResponseFormat(user models.User) map[string]interface{} {
	log.Printf("Formatting response for user: %+v", user)
	
	return map[string]interface{}{
		"id": user.ID,
		"username": user.Username,
		"discriminator": user.Discriminator,
		"display_name": user.DisplayName,
		"avatar": user.Avatar,
		"banner": user.Banner,
		"bot": user.Bot,
		"system": user.System,
		"bio": user.Bio,
		"flags": user.Flags,
		"about_me": user.AboutMe,
		"accent_color": user.AccentColor,
		"presence": user.Presence,
		"created_at": user.CreatedAt,
	}
}
