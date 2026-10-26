// Package oauth is the OAuth 2.1 authorization server that lets AI assistants
// (Claude, ChatGPT, ...) connect to the MCP server on a user's behalf.
//
// Clients are public: they register themselves (RFC 7591) or are identified by
// a client ID metadata document URL, and every grant uses PKCE (S256). A user
// approves a client for one organization and a set of scopes; that approval is
// a connection, whose rotating access/refresh tokens are stored hashed.
// Pending authorization requests and one-time codes live in Redis.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	AccessTokenTTL  = time.Hour
	RefreshTokenTTL = 30 * 24 * time.Hour
	// codeTTL bounds the redirect back to the client and its token request.
	codeTTL = time.Minute
	// requestTTL bounds how long a user may take to log in and approve.
	requestTTL = 10 * time.Minute

	// Token prefixes tell token kinds apart from each other and from API keys.
	AccessTokenPrefix  = "wmo_at_"
	refreshTokenPrefix = "wmo_rt_"
	codePrefix         = "wmo_ac_"
)

// Store is the authorization server's state.
type Store struct {
	DB    *gorm.DB
	Redis *redis.Client
	// HTTPClient fetches client ID metadata documents. It must refuse to
	// connect to private addresses, like the app's shared webhook client.
	HTTPClient *http.Client
}

// NewStore returns a Store.
func NewStore(db *gorm.DB, rdb *redis.Client, httpClient *http.Client) *Store {
	return &Store{DB: db, Redis: rdb, HTTPClient: httpClient}
}

// Error is an OAuth error response (RFC 6749 §5.2).
type Error struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Description }

// Error codes.
const (
	ErrInvalidRequest       = "invalid_request"
	ErrInvalidClient        = "invalid_client"
	ErrInvalidGrant         = "invalid_grant"
	ErrUnsupportedGrantType = "unsupported_grant_type"
	ErrInvalidScope         = "invalid_scope"
	ErrAccessDenied         = "access_denied"
	ErrInvalidRedirectURI   = "invalid_redirect_uri"
	ErrInvalidClientMeta    = "invalid_client_metadata"
	ErrUnsupportedResponse  = "unsupported_response_type"
)

func oauthErr(code, description string) *Error {
	return &Error{Code: code, Description: description}
}

// newSecret returns prefix followed by 256 random bits, base64url-encoded.
func newSecret(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("oauth: crypto/rand failed: " + err.Error())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// hash is how secrets are stored and looked up.
func hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
