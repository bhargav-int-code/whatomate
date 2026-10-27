package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/oauth"
)

// Principal is the user an MCP request acts as, and what it may do.
type Principal struct {
	UserID       uuid.UUID
	OrgID        uuid.UUID
	Email        string
	RoleID       *uuid.UUID
	IsSuperAdmin bool
	// Client describes the connection for audit entries, e.g. `API key "Claude"`.
	Client string
	Scopes []Scope
}

// HasScope reports whether the connection was granted scope.
func (p *Principal) HasScope(scope Scope) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

const principalKey = "principal"

func principalFrom(info *auth.TokenInfo) *Principal {
	if info == nil {
		return nil
	}
	p, _ := info.Extra[principalKey].(*Principal)
	return p
}

// verifyToken resolves a bearer token to a Principal: an OAuth access token
// acts as the user who approved it, with the scopes they granted; an API key
// acts as its creator with every scope, exactly like the REST API.
func (s *Server) verifyToken(_ context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
	switch {
	case strings.HasPrefix(token, oauth.AccessTokenPrefix):
		return s.verifyAccessToken(token, r)
	case !strings.HasPrefix(token, "whm_"):
		return nil, fmt.Errorf("%w: unrecognized token", auth.ErrInvalidToken)
	}
	key, ok := middleware.AuthenticateAPIKey(s.app.DB, token)
	if !ok || !key.User.IsActive {
		return nil, fmt.Errorf("%w: invalid or expired API key", auth.ErrInvalidToken)
	}
	p := &Principal{
		UserID:       key.UserID,
		OrgID:        key.OrganizationID,
		Email:        key.User.Email,
		RoleID:       key.User.RoleID,
		IsSuperAdmin: key.User.IsSuperAdmin,
		Client:       fmt.Sprintf("API key %q", key.Name),
		Scopes:       allScopes,
	}
	info := &auth.TokenInfo{
		Scopes: scopeNames(p.Scopes),
		UserID: p.UserID.String(),
		Extra:  map[string]any{principalKey: p},
	}
	if key.ExpiresAt != nil {
		info.Expiration = *key.ExpiresAt
	}
	return info, nil
}

// verifyAccessToken resolves an OAuth access token. The token must have been
// issued for this endpoint (RFC 8707 audience), and its user must still be
// active and in the organization.
func (s *Server) verifyAccessToken(token string, r *http.Request) (*auth.TokenInfo, error) {
	grant, err := s.oauth.LookupAccessToken(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
	}
	conn := grant.Connection
	if conn.Resource != oauth.NormalizeResource(s.baseURL(r)+Path) {
		return nil, fmt.Errorf("%w: token was issued for another server", auth.ErrInvalidToken)
	}
	member, err := s.oauth.CheckMembership(conn.UserID, conn.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
	}
	client := conn.ClientName
	if client == "" {
		client = "an OAuth app"
	}
	p := &Principal{
		UserID:       conn.UserID,
		OrgID:        conn.OrganizationID,
		Email:        member.User.Email,
		RoleID:       member.RoleID,
		IsSuperAdmin: member.User.IsSuperAdmin,
		Client:       client,
		Scopes:       oauth.Split(conn.Scopes),
	}
	return &auth.TokenInfo{
		Scopes:     scopeNames(p.Scopes),
		Expiration: grant.ExpiresAt,
		UserID:     p.UserID.String(),
		Extra:      map[string]any{principalKey: p},
	}, nil
}

// apiKeyHeaderAsBearer accepts the REST API's X-API-Key header as well as a
// bearer token, so existing API-key setups work unchanged.
func apiKeyHeaderAsBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("X-API-Key"); key != "" && r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		next.ServeHTTP(w, r)
	})
}

// requireEnabled rejects requests for organizations that have not turned on
// MCP access.
func (s *Server) requireEnabled(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(auth.TokenInfoFromContext(r.Context()))
		if p == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.orgEnabled(p.OrgID) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "access_denied",
				"error_description": "AI assistant (MCP) access is turned off for this organization. An admin can turn it on in Settings → General.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) orgEnabled(orgID uuid.UUID) bool {
	enabled, err := s.oauth.MCPEnabled(orgID)
	return err == nil && enabled
}
