package services

import (
	"log"
	"strconv"

	"github.com/scylladb/gocqlx/v2"
	"github.com/scylladb/gocqlx/v2/qb"
)

// SpaceMembershipService handles space membership operations
type SpaceMembershipService struct {
	session *gocqlx.Session
}

// NewSpaceMembershipService creates a new SpaceMembershipService
func NewSpaceMembershipService(session *gocqlx.Session) *SpaceMembershipService {
	return &SpaceMembershipService{session: session}
}

// IsSpaceMember checks if a user is a member of a space
func (s *SpaceMembershipService) IsSpaceMember(spaceID int64, userID string) bool {
	return s.checkMembershipDirect(spaceID, userID) || s.checkMembershipByUser(spaceID, userID)
}

// IsSpaceOwner checks if a user is the owner of a space
func (s *SpaceMembershipService) IsSpaceOwner(spaceID int64, userID string) bool {
	var ownerID string
	query := qb.Select("spaces").Columns("owner_id").Where(qb.Eq("id")).Query(*s.session)
	if err := query.Bind(spaceID).GetRelease(&ownerID); err != nil {
		log.Printf("Error checking space ownership: %v", err)
		return false
	}
	return ownerID == userID
}

// checkMembershipDirect checks membership in space_members table
func (s *SpaceMembershipService) checkMembershipDirect(spaceID int64, userID string) bool {
	var count int
	query := qb.Select("space_members").CountAll().Where(qb.Eq("space_id"), qb.Eq("user_id")).Query(*s.session)
	if err := query.Bind(spaceID, userID).GetRelease(&count); err != nil {
		log.Printf("Error checking direct membership: %v", err)
		return false
	}
	return count > 0
}

// checkMembershipByUser checks membership in space_members_by_user table
func (s *SpaceMembershipService) checkMembershipByUser(spaceID int64, userID string) bool {
	var count int
	// space_id is bigint in space_members_by_user table, not text
	query := qb.Select("space_members_by_user").CountAll().Where(qb.Eq("user_id"), qb.Eq("space_id")).Query(*s.session)
	if err := query.Bind(userID, spaceID).GetRelease(&count); err != nil {
		log.Printf("Error checking membership by user: %v", err)
		return false
	}
	return count > 0
}

// normalizeUserID ensures userID is a string
func (s *SpaceMembershipService) normalizeUserID(userID interface{}) string {
	switch v := userID.(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	default:
		return ""
	}
}

// DebugSpaceMembership provides detailed membership debugging information
func (s *SpaceMembershipService) DebugSpaceMembership(spaceID int64, userID string) {
	log.Printf("[DEBUG] Checking membership for user %s in space %d", userID, spaceID)

	// Check direct membership
	directMember := s.checkMembershipDirect(spaceID, userID)
	log.Printf("[DEBUG] Direct membership (space_members): %t", directMember)

	// Check membership by user
	userMember := s.checkMembershipByUser(spaceID, userID)
	log.Printf("[DEBUG] User membership (space_members_by_user): %t", userMember)

	// Check space ownership
	isOwner := s.IsSpaceOwner(spaceID, userID)
	log.Printf("[DEBUG] Is space owner: %t", isOwner)

	// Get all members for debugging
	var members []string
	query := qb.Select("space_members").Columns("user_id").Where(qb.Eq("space_id")).Query(*s.session)
	if err := query.Bind(spaceID).SelectRelease(&members); err != nil {
		log.Printf("[DEBUG] Error querying space members: %v", err)
		return
	}

	log.Printf("[DEBUG] All space members: %v", members)
	
	// Check what spaces this user is actually a member of
	var userSpaces []int64
	userSpacesQuery := qb.Select("space_members_by_user").Columns("space_id").Where(qb.Eq("user_id")).Query(*s.session)
	if err := userSpacesQuery.Bind(userID).SelectRelease(&userSpaces); err != nil {
		log.Printf("[DEBUG] Error querying user spaces: %v", err)
	} else {
		log.Printf("[DEBUG] User %s is a member of spaces: %v", userID, userSpaces)
	}
	
	log.Printf("[DEBUG] Final membership result: %t", directMember || userMember)
}