package models

import (
	"time"

	"github.com/google/uuid"
)

// OAuthClient is an application that may ask users for access through OAuth,
// such as Claude or ChatGPT. It either registered itself (RFC 7591 dynamic
// client registration, ClientID is random) or is described by a metadata
// document at its ClientID URL, cached here. Clients are not tied to an
// organization: anyone can register one, and only a user's consent grants it
// access. Rows are hard-deleted.
type OAuthClient struct {
	ID           uuid.UUID   `gorm:"type:uuid;primary_key;default:gen_random_uuid()" json:"id"`
	CreatedAt    time.Time   `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt    time.Time   `gorm:"autoUpdateTime" json:"updated_at"`
	ClientID     string      `gorm:"size:512;uniqueIndex;not null" json:"client_id"`
	Name         string      `gorm:"size:255" json:"name"`
	URI          string      `gorm:"size:512" json:"uri"`
	LogoURI      string      `gorm:"size:512" json:"logo_uri"`
	RedirectURIs StringArray `gorm:"type:jsonb;not null" json:"redirect_uris"`
	// FromMetadataDocument marks a client whose details were fetched from its
	// ClientID URL; FetchedAt says when, so the cache can be refreshed.
	FromMetadataDocument bool       `gorm:"default:false" json:"from_metadata_document"`
	FetchedAt            *time.Time `json:"fetched_at,omitempty"`
	// LastAuthorizedAt is set when a user first approves the client.
	// Registered clients that are never approved are deleted after a day.
	LastAuthorizedAt *time.Time `json:"last_authorized_at,omitempty"`
}

func (OAuthClient) TableName() string {
	return "oauth_clients"
}

// OAuthConnection is a user's approval of a client for one organization: what
// the user sees and revokes as a "connected app". Its tokens die with it.
type OAuthConnection struct {
	ID             uuid.UUID  `gorm:"type:uuid;primary_key;default:gen_random_uuid()" json:"id"`
	CreatedAt      time.Time  `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt      time.Time  `gorm:"autoUpdateTime" json:"updated_at"`
	OrganizationID uuid.UUID  `gorm:"type:uuid;index;not null" json:"organization_id"`
	UserID         uuid.UUID  `gorm:"type:uuid;index;not null" json:"user_id"`
	ClientID       string     `gorm:"size:512;index;not null" json:"client_id"`
	ClientName     string     `gorm:"size:255" json:"client_name"`
	Scopes         string     `gorm:"size:255;not null" json:"scopes"`   // space-separated
	Resource       string     `gorm:"size:512;not null" json:"resource"` // the MCP endpoint the tokens are for
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	RevokedAt      *time.Time `gorm:"index" json:"revoked_at,omitempty"`
}

func (OAuthConnection) TableName() string {
	return "oauth_connections"
}

// OAuthToken is one access/refresh token pair of a connection. Only SHA-256
// hashes of the tokens are stored. Using a refresh token rotates it: the row
// is marked rotated and a new pair is issued.
type OAuthToken struct {
	ID               uuid.UUID  `gorm:"type:uuid;primary_key;default:gen_random_uuid()" json:"id"`
	CreatedAt        time.Time  `gorm:"autoCreateTime" json:"created_at"`
	ConnectionID     uuid.UUID  `gorm:"type:uuid;index;not null" json:"connection_id"`
	AccessHash       string     `gorm:"size:64;uniqueIndex;not null" json:"-"`
	RefreshHash      string     `gorm:"size:64;uniqueIndex;not null" json:"-"`
	AccessExpiresAt  time.Time  `gorm:"not null" json:"access_expires_at"`
	RefreshExpiresAt time.Time  `gorm:"index;not null" json:"refresh_expires_at"`
	RotatedAt        *time.Time `json:"rotated_at,omitempty"`
}

func (OAuthToken) TableName() string {
	return "oauth_tokens"
}
