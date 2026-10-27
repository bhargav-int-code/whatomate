package oauth

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// Member is a user acting in an organization.
type Member struct {
	User *models.User
	// RoleID is the user's role in the organization.
	RoleID *uuid.UUID
}

// CheckAccess verifies that userID may connect AI assistants to orgID: the
// user is active and belongs to the organization (or is a super admin), and
// the organization has MCP access turned on. It is checked when a grant is
// approved, on every token issue and on every MCP request, so deactivating a
// user, removing them from the organization or turning MCP off cuts off
// their connections at once.
func (s *Store) CheckAccess(userID, orgID uuid.UUID) (*Member, error) {
	m, err := s.CheckMembership(userID, orgID)
	if err != nil {
		return nil, err
	}
	enabled, err := s.MCPEnabled(orgID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, oauthErr(ErrAccessDenied, "AI assistant (MCP) access is turned off for this organization; an admin can turn it on in Settings → General")
	}
	return m, nil
}

// CheckMembership verifies that userID is active and belongs to orgID (or is
// a super admin), and returns their role there.
func (s *Store) CheckMembership(userID, orgID uuid.UUID) (*Member, error) {
	var user models.User
	if err := s.DB.Where("id = ?", userID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, oauthErr(ErrAccessDenied, "user not found")
		}
		return nil, fmt.Errorf("loading user: %w", err)
	}
	if !user.IsActive {
		return nil, oauthErr(ErrAccessDenied, "user is deactivated")
	}

	m := &Member{User: &user, RoleID: user.RoleID}
	var membership models.UserOrganization
	err := s.DB.Where("user_id = ? AND organization_id = ?", userID, orgID).First(&membership).Error
	switch {
	case err == nil:
		if membership.RoleID != nil {
			m.RoleID = membership.RoleID
		}
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return nil, fmt.Errorf("loading membership: %w", err)
	case user.OrganizationID != orgID && !user.IsSuperAdmin:
		return nil, oauthErr(ErrAccessDenied, "user is not a member of this organization")
	}
	return m, nil
}

// MCPEnabled reports whether the organization allows AI assistant access.
func (s *Store) MCPEnabled(orgID uuid.UUID) (bool, error) {
	var org models.Organization
	if err := s.DB.Select("id", "settings").Where("id = ?", orgID).First(&org).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("loading organization: %w", err)
	}
	enabled, _ := org.Settings["mcp_enabled"].(bool)
	return enabled, nil
}
