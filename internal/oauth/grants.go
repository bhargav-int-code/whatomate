package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	requestKeyPrefix  = "oauth:request:"
	codeKeyPrefix     = "oauth:code:"
	usedCodeKeyPrefix = "oauth:code-used:"
	// usedCodeTTL is how long a redeemed code is remembered, so that a
	// replay can revoke what the first redemption issued.
	usedCodeTTL = 10 * time.Minute
)

// AuthorizationRequest is a validated /oauth/authorize request waiting for the
// user to log in and approve or deny it.
type AuthorizationRequest struct {
	ID            string    `json:"id"`
	ClientID      string    `json:"client_id"`
	ClientName    string    `json:"client_name"`
	ClientURI     string    `json:"client_uri"`
	LogoURI       string    `json:"logo_uri"`
	RedirectURI   string    `json:"redirect_uri"`
	State         string    `json:"state"`
	CodeChallenge string    `json:"code_challenge"`
	Scopes        []Scope   `json:"scopes"`
	Resource      string    `json:"resource"`
	Issuer        string    `json:"issuer"`
	CreatedAt     time.Time `json:"created_at"`
}

// codeGrant is what an authorization code stands for.
type codeGrant struct {
	ClientID      string    `json:"client_id"`
	ClientName    string    `json:"client_name"`
	RedirectURI   string    `json:"redirect_uri"`
	CodeChallenge string    `json:"code_challenge"`
	Scopes        []Scope   `json:"scopes"`
	Resource      string    `json:"resource"`
	UserID        uuid.UUID `json:"user_id"`
	OrgID         uuid.UUID `json:"organization_id"`
}

// SaveRequest stores a pending authorization request and returns its ID.
func (s *Store) SaveRequest(ctx context.Context, req *AuthorizationRequest) (string, error) {
	req.ID = uuid.NewString()
	req.CreatedAt = time.Now()
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	if err := s.Redis.Set(ctx, requestKeyPrefix+req.ID, b, requestTTL).Err(); err != nil {
		return "", fmt.Errorf("saving authorization request: %w", err)
	}
	return req.ID, nil
}

// Request returns a pending authorization request.
func (s *Store) Request(ctx context.Context, id string) (*AuthorizationRequest, error) {
	return s.loadRequest(ctx, id, false)
}

// takeRequest returns a pending authorization request and deletes it, so it
// can be answered only once.
func (s *Store) takeRequest(ctx context.Context, id string) (*AuthorizationRequest, error) {
	return s.loadRequest(ctx, id, true)
}

func (s *Store) loadRequest(ctx context.Context, id string, take bool) (*AuthorizationRequest, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrRequestNotFound
	}
	var b []byte
	var err error
	if take {
		b, err = s.Redis.GetDel(ctx, requestKeyPrefix+id).Bytes()
	} else {
		b, err = s.Redis.Get(ctx, requestKeyPrefix+id).Bytes()
	}
	if errors.Is(err, redis.Nil) {
		return nil, ErrRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("loading authorization request: %w", err)
	}
	var req AuthorizationRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("decoding authorization request: %w", err)
	}
	return &req, nil
}

// ErrRequestNotFound means the authorization request expired, was already
// answered, or never existed.
var ErrRequestNotFound = errors.New("authorization request not found or expired; start connecting again from your AI assistant")

// Approve answers a pending request with the user's consent for orgID and the
// chosen scopes (narrowed to those requested), and returns the URL to send
// the browser back to the client with an authorization code.
func (s *Store) Approve(ctx context.Context, requestID string, userID, orgID uuid.UUID, chosen []Scope) (string, error) {
	req, err := s.Request(ctx, requestID)
	if err != nil {
		return "", err
	}
	scopes := Intersect(req.Scopes, chosen)
	if len(scopes) == 0 {
		return "", oauthErr(ErrInvalidScope, "choose at least one permission to grant")
	}
	if _, err := s.CheckAccess(userID, orgID); err != nil {
		return "", err
	}
	// Consume the request only once the approval is known to be valid, so a
	// user can fix their choice and try again
	if req, err = s.takeRequest(ctx, requestID); err != nil {
		return "", err
	}

	code := newSecret(codePrefix)
	grant := codeGrant{
		ClientID:      req.ClientID,
		ClientName:    req.ClientName,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
		Scopes:        scopes,
		Resource:      req.Resource,
		UserID:        userID,
		OrgID:         orgID,
	}
	b, err := json.Marshal(grant)
	if err != nil {
		return "", err
	}
	if err := s.Redis.Set(ctx, codeKeyPrefix+hash(code), b, codeTTL).Err(); err != nil {
		return "", fmt.Errorf("saving authorization code: %w", err)
	}
	now := time.Now()
	s.DB.Table("oauth_clients").Where("client_id = ?", req.ClientID).Update("last_authorized_at", now)

	return redirectWith(req, url.Values{"code": {code}}), nil
}

// Deny answers a pending request with a refusal and returns the URL to send
// the browser back to the client with.
func (s *Store) Deny(ctx context.Context, requestID string) (string, error) {
	req, err := s.takeRequest(ctx, requestID)
	if err != nil {
		return "", err
	}
	return redirectWith(req, url.Values{
		"error":             {ErrAccessDenied},
		"error_description": {"The user denied access"},
	}), nil
}

// RedirectError returns the client's redirect URI carrying an error, for
// authorize requests that fail after the redirect URI was validated.
func RedirectError(redirectURI, state, issuer string, e *Error) string {
	return redirectWith(&AuthorizationRequest{RedirectURI: redirectURI, State: state, Issuer: issuer},
		url.Values{"error": {e.Code}, "error_description": {e.Description}})
}

// redirectWith adds params, state and iss (RFC 9207) to the request's
// redirect URI.
func redirectWith(req *AuthorizationRequest, params url.Values) string {
	u, err := url.Parse(req.RedirectURI)
	if err != nil {
		return req.RedirectURI
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	q.Set("iss", req.Issuer)
	u.RawQuery = q.Encode()
	return u.String()
}

// takeCode redeems an authorization code. A code that was already redeemed
// revokes the connection it produced (RFC 6749 §10.5).
func (s *Store) takeCode(ctx context.Context, code string) (*codeGrant, error) {
	key := hash(code)
	b, err := s.Redis.GetDel(ctx, codeKeyPrefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		if connID, err := s.Redis.Get(ctx, usedCodeKeyPrefix+key).Result(); err == nil {
			if id, err := uuid.Parse(connID); err == nil {
				s.revokeConnectionByID(id, uuid.Nil, "authorization code reused")
			}
		}
		return nil, oauthErr(ErrInvalidGrant, "authorization code is invalid, expired or already used")
	}
	if err != nil {
		return nil, fmt.Errorf("loading authorization code: %w", err)
	}
	var grant codeGrant
	if err := json.Unmarshal(b, &grant); err != nil {
		return nil, fmt.Errorf("decoding authorization code: %w", err)
	}
	return &grant, nil
}

func (s *Store) markCodeUsed(ctx context.Context, code string, connectionID uuid.UUID) {
	s.Redis.Set(ctx, usedCodeKeyPrefix+hash(code), connectionID.String(), usedCodeTTL)
}

// verifierRe is RFC 7636's code_verifier syntax.
var verifierRe = regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`)

// ValidChallenge reports whether challenge looks like an S256 code challenge.
func ValidChallenge(challenge string) bool {
	return len(challenge) == 43 && verifierRe.MatchString(challenge)
}

// verifyPKCE checks verifier against an S256 challenge.
func verifyPKCE(verifier, challenge string) bool {
	if !verifierRe.MatchString(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}
