package handlers

import (
	"errors"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/oauth"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// OAuthRequestResponse describes a pending authorization request on the
// consent page.
type OAuthRequestResponse struct {
	ID            string                 `json:"id"`
	ClientName    string                 `json:"client_name"`
	ClientURI     string                 `json:"client_uri,omitempty"`
	LogoURI       string                 `json:"logo_uri,omitempty"`
	RedirectHost  string                 `json:"redirect_host"`
	Scopes        []string               `json:"scopes"`
	Organizations []OAuthConsentOrgEntry `json:"organizations"`
}

// OAuthConsentOrgEntry is an organization the user can connect the app to.
type OAuthConsentOrgEntry struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	MCPEnabled bool      `json:"mcp_enabled"`
	IsCurrent  bool      `json:"is_current"`
}

// OAuthConnectionResponse is a connected app.
type OAuthConnectionResponse struct {
	ID               uuid.UUID  `json:"id"`
	ClientName       string     `json:"client_name"`
	ClientID         string     `json:"client_id"`
	Scopes           []string   `json:"scopes"`
	OrganizationID   uuid.UUID  `json:"organization_id"`
	OrganizationName string     `json:"organization_name"`
	UserID           uuid.UUID  `json:"user_id"`
	UserName         string     `json:"user_name"`
	UserEmail        string     `json:"user_email"`
	CreatedAt        time.Time  `json:"created_at"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
}

func (a *App) oauthStore() *oauth.Store {
	return oauth.NewStore(a.DB, a.Redis, a.HTTPClient)
}

// GetOAuthRequest returns a pending authorization request for the consent
// page, with the organizations the user may connect it to.
func (a *App) GetOAuthRequest(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	req, err := a.oauthStore().Request(r.RequestCtx, r.RequestCtx.UserValue("id").(string))
	if err != nil {
		return a.sendOAuthError(r, err)
	}

	resp := OAuthRequestResponse{
		ID:         req.ID,
		ClientName: req.ClientName,
		ClientURI:  req.ClientURI,
		LogoURI:    req.LogoURI,
		Scopes:     make([]string, len(req.Scopes)),
	}
	if resp.ClientName == "" {
		resp.ClientName = "An unnamed app"
	}
	if u, err := url.Parse(req.RedirectURI); err == nil {
		resp.RedirectHost = u.Host
	}
	for i, s := range req.Scopes {
		resp.Scopes[i] = string(s)
	}

	orgs, err := a.consentOrganizations(userID, orgID)
	if err != nil {
		a.Log.Error("Failed to list organizations for consent", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load organizations", nil, "")
	}
	resp.Organizations = orgs
	return r.SendEnvelope(resp)
}

// consentOrganizations lists the organizations userID belongs to.
func (a *App) consentOrganizations(userID, currentOrgID uuid.UUID) ([]OAuthConsentOrgEntry, error) {
	var orgs []models.Organization
	err := a.DB.Where("id IN (SELECT organization_id FROM user_organizations WHERE user_id = ? AND deleted_at IS NULL) OR id = ?", userID, currentOrgID).
		Order("name").Find(&orgs).Error
	if err != nil {
		return nil, err
	}
	out := make([]OAuthConsentOrgEntry, len(orgs))
	for i, o := range orgs {
		enabled, _ := o.Settings["mcp_enabled"].(bool)
		out[i] = OAuthConsentOrgEntry{ID: o.ID, Name: o.Name, MCPEnabled: enabled, IsCurrent: o.ID == currentOrgID}
	}
	return out, nil
}

// ApproveOAuthRequest grants a pending request for the chosen organization
// and scopes, and returns where to send the browser.
func (a *App) ApproveOAuthRequest(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	var body struct {
		OrganizationID *uuid.UUID `json:"organization_id"`
		Scopes         []string   `json:"scopes"`
	}
	if err := a.decodeRequest(r, &body); err != nil {
		return nil
	}
	if body.OrganizationID != nil {
		orgID = *body.OrganizationID
	}
	scopes := make([]oauth.Scope, len(body.Scopes))
	for i, s := range body.Scopes {
		scopes[i] = oauth.Scope(s)
	}

	redirectURL, err := a.oauthStore().Approve(r.RequestCtx, r.RequestCtx.UserValue("id").(string), userID, orgID, scopes)
	if err != nil {
		return a.sendOAuthError(r, err)
	}
	return r.SendEnvelope(map[string]string{"redirect_url": redirectURL})
}

// DenyOAuthRequest refuses a pending request and returns where to send the
// browser.
func (a *App) DenyOAuthRequest(r *fastglue.Request) error {
	if _, _, err := a.getOrgAndUserID(r); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	redirectURL, err := a.oauthStore().Deny(r.RequestCtx, r.RequestCtx.UserValue("id").(string))
	if err != nil {
		return a.sendOAuthError(r, err)
	}
	return r.SendEnvelope(map[string]string{"redirect_url": redirectURL})
}

func (a *App) sendOAuthError(r *fastglue.Request, err error) error {
	if errors.Is(err, oauth.ErrRequestNotFound) {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, err.Error(), nil, "")
	}
	var oe *oauth.Error
	if errors.As(err, &oe) {
		status := fasthttp.StatusBadRequest
		if oe.Code == oauth.ErrAccessDenied {
			status = fasthttp.StatusForbidden
		}
		return r.SendErrorEnvelope(status, oe.Description, nil, "")
	}
	a.Log.Error("OAuth consent failed", "error", err)
	return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Something went wrong", nil, "")
}

// ListMyConnectedApps lists the AI assistants the current user has connected.
func (a *App) ListMyConnectedApps(r *fastglue.Request) error {
	_, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	conns, err := a.listConnections("c.user_id = ?", userID)
	if err != nil {
		a.Log.Error("Failed to list connected apps", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list connected apps", nil, "")
	}
	return r.SendEnvelope(map[string]any{"connections": conns})
}

// RevokeMyConnectedApp disconnects one of the current user's apps.
func (a *App) RevokeMyConnectedApp(r *fastglue.Request) error {
	_, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	return a.revokeConnection(r, userID, "user_id = ?", userID)
}

// ListMCPConnections lists every connected AI assistant in the organization.
func (a *App) ListMCPConnections(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceSettingsGeneral, models.ActionWrite)
	if err != nil {
		return nil
	}
	conns, err := a.listConnections("c.organization_id = ?", orgID)
	if err != nil {
		a.Log.Error("Failed to list MCP connections", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list connections", nil, "")
	}
	return r.SendEnvelope(map[string]any{"connections": conns})
}

// RevokeMCPConnection disconnects any app in the organization.
func (a *App) RevokeMCPConnection(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceSettingsGeneral, models.ActionWrite)
	if err != nil {
		return nil
	}
	return a.revokeConnection(r, userID, "organization_id = ?", orgID)
}

// revokeConnection revokes the {id} connection if it matches scope.
func (a *App) revokeConnection(r *fastglue.Request, actorID uuid.UUID, scope string, scopeArg any) error {
	id, err := parsePathUUID(r, "id", "connection")
	if err != nil {
		return nil
	}
	var conn models.OAuthConnection
	if err := a.DB.Where("id = ? AND revoked_at IS NULL", id).Where(scope, scopeArg).First(&conn).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Connection not found", nil, "")
	}
	a.oauthStore().RevokeConnection(&conn, actorID)
	return r.SendEnvelope(map[string]string{"message": "Disconnected"})
}

// listConnections returns active connections matching where, newest first.
func (a *App) listConnections(where string, arg any) ([]OAuthConnectionResponse, error) {
	var rows []struct {
		models.OAuthConnection
		OrganizationName string
		UserName         string
		UserEmail        string
	}
	err := a.DB.Table("oauth_connections c").
		Select("c.*, o.name AS organization_name, u.full_name AS user_name, u.email AS user_email").
		Joins("JOIN organizations o ON o.id = c.organization_id").
		Joins("JOIN users u ON u.id = c.user_id").
		Where("c.revoked_at IS NULL").Where(where, arg).
		Order("c.created_at DESC").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]OAuthConnectionResponse, len(rows))
	for i, row := range rows {
		scopes := oauth.Split(row.Scopes)
		names := make([]string, len(scopes))
		for j, s := range scopes {
			names[j] = string(s)
		}
		out[i] = OAuthConnectionResponse{
			ID:               row.ID,
			ClientName:       row.ClientName,
			ClientID:         row.ClientID,
			Scopes:           names,
			OrganizationID:   row.OrganizationID,
			OrganizationName: row.OrganizationName,
			UserID:           row.UserID,
			UserName:         row.UserName,
			UserEmail:        row.UserEmail,
			CreatedAt:        row.CreatedAt,
			LastUsedAt:       row.LastUsedAt,
		}
	}
	return out, nil
}
