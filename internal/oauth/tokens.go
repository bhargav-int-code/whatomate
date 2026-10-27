package oauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

const (
	// rotationGrace tolerates a client sending the same refresh token twice
	// at once (e.g. parallel requests): the second gets invalid_grant
	// without revoking the connection. Reuse after this is treated as theft.
	rotationGrace = 10 * time.Second
	// lastUsedEvery throttles last_used_at updates.
	lastUsedEvery = time.Minute
	// revokedRetention keeps revoked connections listed for a while.
	revokedRetention = 30 * 24 * time.Hour
	// unusedClientTTL deletes registered clients no user ever approved.
	unusedClientTTL = 24 * time.Hour

	// AuditResource is the audit log resource type for connections.
	AuditResource = "mcp_connections"
)

// TokenResponse is a successful token endpoint response (RFC 6749 §5.1).
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// ExchangeCode redeems an authorization code for the first token pair of a
// new connection.
func (s *Store) ExchangeCode(ctx context.Context, code, clientID, redirectURI, verifier, resource string) (*TokenResponse, error) {
	if code == "" || clientID == "" || verifier == "" {
		return nil, oauthErr(ErrInvalidRequest, "code, client_id and code_verifier are required")
	}
	grant, err := s.takeCode(ctx, code)
	if err != nil {
		return nil, err
	}
	switch {
	case grant.ClientID != clientID:
		return nil, oauthErr(ErrInvalidGrant, "code was issued to another client")
	case grant.RedirectURI != redirectURI:
		return nil, oauthErr(ErrInvalidGrant, "redirect_uri does not match the authorization request")
	case !verifyPKCE(verifier, grant.CodeChallenge):
		return nil, oauthErr(ErrInvalidGrant, "code_verifier does not match the code challenge")
	case resource != "" && NormalizeResource(resource) != grant.Resource:
		return nil, oauthErr(ErrInvalidGrant, "resource does not match the authorization request")
	}
	if _, err := s.CheckAccess(grant.UserID, grant.OrgID); err != nil {
		return nil, err
	}

	conn := &models.OAuthConnection{
		OrganizationID: grant.OrgID,
		UserID:         grant.UserID,
		ClientID:       grant.ClientID,
		ClientName:     grant.ClientName,
		Scopes:         Join(grant.Scopes),
		Resource:       grant.Resource,
	}
	var resp *TokenResponse
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(conn).Error; err != nil {
			return err
		}
		var err error
		resp, err = issuePair(tx, conn)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("issuing tokens: %w", err)
	}
	s.markCodeUsed(ctx, code, conn.ID)

	audit.LogAudit(s.DB, conn.OrganizationID, conn.UserID, audit.GetUserName(s.DB, conn.UserID),
		AuditResource, conn.ID, models.AuditActionCreated, nil, nil,
		map[string]any{"field": "app", "old_value": nil, "new_value": clientLabel(conn)},
		map[string]any{"field": "scopes", "old_value": nil, "new_value": strings.ReplaceAll(conn.Scopes, " ", ", ")})
	return resp, nil
}

// Refresh rotates a refresh token: it is used up and a new pair is issued.
// Presenting a used refresh token again revokes the whole connection, since
// it means the token leaked (OAuth 2.1 §4.3.1).
func (s *Store) Refresh(refreshToken, clientID, scopeParam string) (*TokenResponse, error) {
	if refreshToken == "" || clientID == "" {
		return nil, oauthErr(ErrInvalidRequest, "refresh_token and client_id are required")
	}
	invalid := oauthErr(ErrInvalidGrant, "refresh token is invalid, expired or revoked")

	var tok models.OAuthToken
	if err := s.DB.Where("refresh_hash = ?", hash(refreshToken)).First(&tok).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, invalid
		}
		return nil, fmt.Errorf("loading token: %w", err)
	}
	var conn models.OAuthConnection
	if err := s.DB.Where("id = ?", tok.ConnectionID).First(&conn).Error; err != nil {
		return nil, invalid
	}
	if conn.RevokedAt != nil || conn.ClientID != clientID || time.Now().After(tok.RefreshExpiresAt) {
		return nil, invalid
	}
	if tok.RotatedAt != nil {
		if time.Since(*tok.RotatedAt) > rotationGrace {
			s.revokeConnection(&conn, uuid.Nil, "refresh token reused")
		}
		return nil, invalid
	}
	if scopeParam != "" {
		requested, err := ParseScopes(scopeParam)
		if err != nil {
			return nil, err
		}
		if len(Intersect(requested, Split(conn.Scopes))) != len(requested) {
			return nil, oauthErr(ErrInvalidScope, "a refresh cannot add scopes the user did not grant")
		}
	}
	if _, err := s.CheckAccess(conn.UserID, conn.OrganizationID); err != nil {
		return nil, err
	}

	var resp *TokenResponse
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		// Claim the token atomically, so two concurrent refreshes cannot
		// both succeed
		res := tx.Model(&models.OAuthToken{}).Where("id = ? AND rotated_at IS NULL", tok.ID).Update("rotated_at", time.Now())
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return invalid
		}
		var err error
		resp, err = issuePair(tx, &conn)
		return err
	})
	if err != nil {
		var oe *Error
		if errors.As(err, &oe) {
			return nil, oe
		}
		return nil, fmt.Errorf("rotating tokens: %w", err)
	}
	return resp, nil
}

// issuePair creates a token pair for conn.
func issuePair(tx *gorm.DB, conn *models.OAuthConnection) (*TokenResponse, error) {
	access := newSecret(AccessTokenPrefix)
	refresh := newSecret(refreshTokenPrefix)
	now := time.Now()
	tok := models.OAuthToken{
		ConnectionID:     conn.ID,
		AccessHash:       hash(access),
		RefreshHash:      hash(refresh),
		AccessExpiresAt:  now.Add(AccessTokenTTL),
		RefreshExpiresAt: now.Add(RefreshTokenTTL),
	}
	if err := tx.Create(&tok).Error; err != nil {
		return nil, err
	}
	return &TokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int(AccessTokenTTL.Seconds()),
		RefreshToken: refresh,
		Scope:        conn.Scopes,
	}, nil
}

// AccessGrant is what a valid access token allows.
type AccessGrant struct {
	Connection models.OAuthConnection
	ExpiresAt  time.Time
}

// LookupAccessToken returns the connection an access token belongs to. It
// fails for unknown, expired and revoked tokens.
func (s *Store) LookupAccessToken(token string) (*AccessGrant, error) {
	var tok models.OAuthToken
	if err := s.DB.Where("access_hash = ?", hash(token)).First(&tok).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("unknown access token")
		}
		return nil, fmt.Errorf("loading token: %w", err)
	}
	if time.Now().After(tok.AccessExpiresAt) {
		return nil, errors.New("access token expired")
	}
	var conn models.OAuthConnection
	if err := s.DB.Where("id = ?", tok.ConnectionID).First(&conn).Error; err != nil {
		return nil, errors.New("connection not found")
	}
	if conn.RevokedAt != nil {
		return nil, errors.New("connection revoked")
	}
	if conn.LastUsedAt == nil || time.Since(*conn.LastUsedAt) > lastUsedEvery {
		now := time.Now()
		s.DB.Model(&models.OAuthConnection{}).Where("id = ?", conn.ID).Update("last_used_at", now)
		conn.LastUsedAt = &now
	}
	return &AccessGrant{Connection: conn, ExpiresAt: tok.AccessExpiresAt}, nil
}

// RevokeToken revokes the connection of an access or refresh token (RFC
// 7009). Unknown tokens are ignored, as the RFC requires.
func (s *Store) RevokeToken(token, clientID string) {
	var tok models.OAuthToken
	h := hash(token)
	if err := s.DB.Where("access_hash = ? OR refresh_hash = ?", h, h).First(&tok).Error; err != nil {
		return
	}
	var conn models.OAuthConnection
	if err := s.DB.Where("id = ?", tok.ConnectionID).First(&conn).Error; err != nil {
		return
	}
	if clientID != "" && conn.ClientID != clientID {
		return
	}
	s.revokeConnection(&conn, conn.UserID, "revoked by the app")
}

// RevokeConnection revokes a connection on behalf of actorID (the user or an
// admin) and deletes its tokens.
func (s *Store) RevokeConnection(conn *models.OAuthConnection, actorID uuid.UUID) {
	s.revokeConnection(conn, actorID, "")
}

func (s *Store) revokeConnectionByID(id, actorID uuid.UUID, reason string) {
	var conn models.OAuthConnection
	if err := s.DB.Where("id = ?", id).First(&conn).Error; err == nil {
		s.revokeConnection(&conn, actorID, reason)
	}
}

// revokeConnection revokes conn and records who did it and why. A zero
// actorID means the server did, e.g. on token reuse.
func (s *Store) revokeConnection(conn *models.OAuthConnection, actorID uuid.UUID, reason string) {
	if conn.RevokedAt != nil {
		return
	}
	now := time.Now()
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.OAuthConnection{}).Where("id = ? AND revoked_at IS NULL", conn.ID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		return tx.Where("connection_id = ?", conn.ID).Delete(&models.OAuthToken{}).Error
	})
	if err != nil {
		return
	}
	conn.RevokedAt = &now

	userID, userName := actorID, ""
	if actorID == uuid.Nil {
		userID, userName = conn.UserID, "System"
	} else {
		userName = audit.GetUserName(s.DB, actorID)
	}
	extra := []map[string]any{{"field": "app", "old_value": clientLabel(conn), "new_value": nil}}
	if reason != "" {
		extra = append(extra, map[string]any{"field": "reason", "old_value": nil, "new_value": reason})
	}
	audit.LogAudit(s.DB, conn.OrganizationID, userID, userName, AuditResource, conn.ID, models.AuditActionDeleted, nil, nil, extra...)
}

func clientLabel(conn *models.OAuthConnection) string {
	if conn.ClientName != "" {
		return conn.ClientName
	}
	return conn.ClientID
}

// Sweep deletes expired tokens, retires connections left without usable
// tokens, and removes long-revoked connections and registered clients that no
// user ever approved.
func (s *Store) Sweep() error {
	now := time.Now()
	// Rotated tokens are kept until they expire, so their reuse is detected
	if err := s.DB.Where("refresh_expires_at < ?", now).Delete(&models.OAuthToken{}).Error; err != nil {
		return fmt.Errorf("deleting expired tokens: %w", err)
	}
	if err := s.DB.Model(&models.OAuthConnection{}).
		Where("revoked_at IS NULL AND created_at < ? AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.connection_id = oauth_connections.id)", now.Add(-time.Hour)).
		Update("revoked_at", now).Error; err != nil {
		return fmt.Errorf("retiring expired connections: %w", err)
	}
	if err := s.DB.Where("revoked_at < ?", now.Add(-revokedRetention)).Delete(&models.OAuthConnection{}).Error; err != nil {
		return fmt.Errorf("deleting old connections: %w", err)
	}
	if err := s.DB.Where("from_metadata_document = ? AND last_authorized_at IS NULL AND created_at < ?", false, now.Add(-unusedClientTTL)).
		Delete(&models.OAuthClient{}).Error; err != nil {
		return fmt.Errorf("deleting unused clients: %w", err)
	}
	return nil
}

// StartSweeper runs Sweep hourly until ctx is done.
func (s *Store) StartSweeper(ctx context.Context, logError func(msg string, args ...any)) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if err := s.Sweep(); err != nil {
			logError("OAuth sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// NormalizeResource canonicalizes a resource indicator (RFC 8707) for
// comparison: no trailing slash, lowercase scheme and host.
func NormalizeResource(resource string) string {
	r := strings.TrimRight(strings.TrimSpace(resource), "/")
	if i := strings.Index(r, "://"); i != -1 {
		rest := r[i+3:]
		host, path, _ := strings.Cut(rest, "/")
		r = strings.ToLower(r[:i]) + "://" + strings.ToLower(host)
		if path != "" {
			r += "/" + path
		}
	}
	return r
}
