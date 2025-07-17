package handlers_v1

import (
	"log"
	"strconv"
	"sync"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"
)

type MutualFriendsResponse struct {
	UserID        string  `json:"user_id"`
	Username      string  `json:"username"`
	Discriminator int     `json:"discriminator"`
	DisplayName   *string `json:"display_name"`
	Avatar        *string `json:"avatar"`
	Bot           bool    `json:"bot"`
}

type MutualSpacesResponse struct {
	SpaceID     string  `json:"space_id"`
	Name        string  `json:"name"`
	Icon        *string `json:"icon"`
	Description *string `json:"description"`
	OwnerID     string  `json:"owner_id"`
	JoinedAt    string  `json:"joined_at"`
}

// GetMutualFriends returns mutual friends between the authenticated user and target user
func GetMutualFriends(c fiber.Ctx) error {
	currentUser := c.Locals("user").(models.User)
	targetUserID := c.Params("id")

	if targetUserID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Target user ID is required",
		})
	}

	if targetUserID == currentUser.ID {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Cannot get mutual friends with yourself",
		})
	}

	// Get current user's friends
	currentUserFriends := currentUser.Relationships
	if len(currentUserFriends) == 0 {
		return c.JSON([]MutualFriendsResponse{})
	}

	// Get target user
	targetUser, err := helpers.GetUserByIDString(targetUserID)
	if err != nil {
		log.Printf("Error getting target user: %v", err)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Target user not found",
		})
	}

	// Get target user's friends
	targetUserFriends := targetUser.Relationships
	if len(targetUserFriends) == 0 {
		return c.JSON([]MutualFriendsResponse{})
	}

	// Find mutual friends
	mutualFriendIDs := make(map[string]bool)
	for _, friendID := range currentUserFriends {
		for _, targetFriendID := range targetUserFriends {
			if friendID == targetFriendID {
				mutualFriendIDs[friendID] = true
				break
			}
		}
	}

	if len(mutualFriendIDs) == 0 {
		return c.JSON([]MutualFriendsResponse{})
	}

	// Get user details for mutual friends
	mutualFriends := make([]MutualFriendsResponse, 0, len(mutualFriendIDs))
	var wg sync.WaitGroup
	var mu sync.Mutex

	for friendID := range mutualFriendIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			friend, err := helpers.GetUserByIDString(id)
			if err != nil {
				log.Printf("Error getting friend user %s: %v", id, err)
				return
			}

			mu.Lock()
			mutualFriends = append(mutualFriends, MutualFriendsResponse{
				UserID:        friend.ID,
				Username:      friend.Username,
				Discriminator: friend.Discriminator,
				DisplayName:   friend.DisplayName,
				Avatar:        friend.Avatar,
				Bot:           friend.Bot,
			})
			mu.Unlock()
		}(friendID)
	}

	wg.Wait()

	return c.JSON(mutualFriends)
}

// GetMutualSpaces returns mutual spaces between the authenticated user and target user
func GetMutualSpaces(c fiber.Ctx) error {
	currentUser := c.Locals("user").(models.User)
	targetUserID := c.Params("id")

	if targetUserID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Target user ID is required",
		})
	}

	if targetUserID == currentUser.ID {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Cannot get mutual spaces with yourself",
		})
	}

	// Get current user's spaces
	var currentUserSpaces []models.SpaceMembersByUser
	currentUserSpacesQuery := qb.Select("space_members_by_user").
		Where(qb.Eq("user_id")).
		Query(*database.Session)

	if err := currentUserSpacesQuery.Bind(currentUser.ID).SelectRelease(&currentUserSpaces); err != nil {
		log.Printf("Error getting current user spaces: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get user spaces",
		})
	}

	if len(currentUserSpaces) == 0 {
		return c.JSON([]MutualSpacesResponse{})
	}

	// Get target user's spaces
	var targetUserSpaces []models.SpaceMembersByUser
	targetUserSpacesQuery := qb.Select("space_members_by_user").
		Where(qb.Eq("user_id")).
		Query(*database.Session)

	if err := targetUserSpacesQuery.Bind(targetUserID).SelectRelease(&targetUserSpaces); err != nil {
		log.Printf("Error getting target user spaces: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get target user spaces",
		})
	}

	if len(targetUserSpaces) == 0 {
		return c.JSON([]MutualSpacesResponse{})
	}

	// Find mutual spaces and store current user's join dates
	mutualSpaceData := make(map[int64]models.SpaceMembersByUser)
	for _, currentSpace := range currentUserSpaces {
		for _, targetSpace := range targetUserSpaces {
			if currentSpace.SpaceID == targetSpace.SpaceID {
				mutualSpaceData[currentSpace.SpaceID] = currentSpace
				break
			}
		}
	}

	if len(mutualSpaceData) == 0 {
		return c.JSON([]MutualSpacesResponse{})
	}

	// Get space details for mutual spaces
	mutualSpaces := make([]MutualSpacesResponse, 0, len(mutualSpaceData))
	var wg sync.WaitGroup
	var mu sync.Mutex

	for spaceID, memberData := range mutualSpaceData {
		wg.Add(1)
		go func(id int64, member models.SpaceMembersByUser) {
			defer wg.Done()
			var space models.Space
			spaceQuery := qb.Select("spaces").
				Where(qb.Eq("id")).
				Query(*database.Session)

			if err := spaceQuery.Bind(id).GetRelease(&space); err != nil {
				log.Printf("Error getting space %d: %v", id, err)
				return
			}

			mu.Lock()
			mutualSpaces = append(mutualSpaces, MutualSpacesResponse{
				SpaceID:     strconv.FormatInt(space.ID, 10),
				Name:        space.Name,
				Icon:        space.Icon,
				Description: space.Description,
				OwnerID:     space.OwnerID,
				JoinedAt:    member.JoinedAt.Format("2006-01-02T15:04:05Z07:00"),
			})
			mu.Unlock()
		}(spaceID, memberData)
	}

	wg.Wait()

	return c.JSON(mutualSpaces)
}