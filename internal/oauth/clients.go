package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

const (
	maxRedirectURIs      = 10
	maxURILength         = 512
	maxNameLength        = 255
	metadataDocMaxBytes  = 64 << 10
	metadataDocTimeout   = 5 * time.Second
	metadataDocCacheTime = 24 * time.Hour
)

// ClientMetadata is a client's registration request (RFC 7591) or its client
// ID metadata document.
type ClientMetadata struct {
	ClientID                string   `json:"client_id,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
}

// RegisterClient registers a public client (RFC 7591). Every client is
// registered with token_endpoint_auth_method "none": no client secret is
// issued, and PKCE protects its grants.
func (s *Store) RegisterClient(md ClientMetadata) (*models.OAuthClient, error) {
	if err := validateRedirectURIs(md.RedirectURIs); err != nil {
		return nil, err
	}
	for _, gt := range md.GrantTypes {
		if gt != "authorization_code" && gt != "refresh_token" {
			return nil, oauthErr(ErrInvalidClientMeta, "unsupported grant type "+gt)
		}
	}
	for _, rt := range md.ResponseTypes {
		if rt != "code" {
			return nil, oauthErr(ErrInvalidClientMeta, "unsupported response type "+rt)
		}
	}
	client := &models.OAuthClient{
		ClientID:     newSecret("wmo_client_"),
		Name:         truncate(strings.TrimSpace(md.ClientName), maxNameLength),
		URI:          optionalHTTPSURL(md.ClientURI),
		LogoURI:      optionalHTTPSURL(md.LogoURI),
		RedirectURIs: md.RedirectURIs,
	}
	if err := s.DB.Create(client).Error; err != nil {
		return nil, fmt.Errorf("saving client: %w", err)
	}
	return client, nil
}

// IsMetadataDocumentClientID reports whether clientID is a client ID metadata
// document URL rather than a registered ID.
func IsMetadataDocumentClientID(clientID string) bool {
	return strings.HasPrefix(clientID, "https://")
}

// Client returns the client with clientID, fetching its metadata document
// when clientID is a URL and the cached copy is missing or stale.
func (s *Store) Client(ctx context.Context, clientID string) (*models.OAuthClient, error) {
	var client models.OAuthClient
	err := s.DB.Where("client_id = ?", clientID).First(&client).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("loading client: %w", err)
	}
	found := err == nil

	if !IsMetadataDocumentClientID(clientID) {
		if !found {
			return nil, oauthErr(ErrInvalidClient, "unknown client_id")
		}
		return &client, nil
	}
	if found && client.FetchedAt != nil && time.Since(*client.FetchedAt) < metadataDocCacheTime {
		return &client, nil
	}

	md, err := s.fetchMetadataDocument(ctx, clientID)
	if err != nil {
		if found {
			// Keep serving the cached copy when its URL is briefly unreachable
			return &client, nil
		}
		return nil, err
	}
	now := time.Now()
	client.ClientID = clientID
	client.Name = truncate(strings.TrimSpace(md.ClientName), maxNameLength)
	client.URI = optionalHTTPSURL(md.ClientURI)
	client.LogoURI = optionalHTTPSURL(md.LogoURI)
	client.RedirectURIs = md.RedirectURIs
	client.FromMetadataDocument = true
	client.FetchedAt = &now
	if found {
		err = s.DB.Save(&client).Error
	} else {
		err = s.DB.Create(&client).Error
	}
	if err != nil {
		return nil, fmt.Errorf("saving client: %w", err)
	}
	return &client, nil
}

// fetchMetadataDocument downloads and validates a client ID metadata
// document. The client ID must be an HTTPS URL with a path, and the document
// must name that same URL as its client_id.
func (s *Store) fetchMetadataDocument(ctx context.Context, clientID string) (*ClientMetadata, error) {
	invalid := func(why string) error {
		return oauthErr(ErrInvalidClient, "client metadata document "+why)
	}
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path == "" || u.Path == "/" || u.Fragment != "" || u.User != nil {
		return nil, invalid("URL must be https with a path")
	}

	ctx, cancel := context.WithTimeout(ctx, metadataDocTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return nil, invalid("URL is invalid")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, invalid("could not be fetched")
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, invalid(fmt.Sprintf("returned HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, metadataDocMaxBytes+1))
	if err != nil {
		return nil, invalid("could not be read")
	}
	if len(body) > metadataDocMaxBytes {
		return nil, invalid("is too large")
	}
	var md ClientMetadata
	if err := json.Unmarshal(body, &md); err != nil {
		return nil, invalid("is not valid JSON")
	}
	if md.ClientID != clientID {
		return nil, invalid("client_id does not match its URL")
	}
	if err := validateRedirectURIs(md.RedirectURIs); err != nil {
		return nil, invalid("has invalid redirect_uris")
	}
	return &md, nil
}

// validateRedirectURIs checks registered redirect URIs: absolute, no
// fragment, and HTTPS unless they point at the user's own machine (native
// apps such as Claude Code listen on a loopback port).
func validateRedirectURIs(uris []string) error {
	if len(uris) == 0 {
		return oauthErr(ErrInvalidRedirectURI, "redirect_uris is required")
	}
	if len(uris) > maxRedirectURIs {
		return oauthErr(ErrInvalidRedirectURI, fmt.Sprintf("at most %d redirect_uris are allowed", maxRedirectURIs))
	}
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil || len(raw) > maxURILength || u.Host == "" || u.Fragment != "" || u.User != nil {
			return oauthErr(ErrInvalidRedirectURI, "invalid redirect URI "+truncate(raw, 100))
		}
		switch {
		case u.Scheme == "https":
		case u.Scheme == "http" && isLoopback(u.Hostname()):
		default:
			return oauthErr(ErrInvalidRedirectURI, "redirect URIs must use https, or http on localhost: "+truncate(raw, 100))
		}
	}
	return nil
}

// RedirectURIAllowed reports whether uri is one of the client's redirect
// URIs. For loopback URIs the port may differ (RFC 8252 §7.3), since native
// apps pick a free port each time.
func RedirectURIAllowed(client *models.OAuthClient, uri string) bool {
	got, err := url.Parse(uri)
	if err != nil || got.Fragment != "" {
		return false
	}
	for _, registered := range client.RedirectURIs {
		if registered == uri {
			return true
		}
		want, err := url.Parse(registered)
		if err != nil || want.Scheme != "http" || !isLoopback(want.Hostname()) {
			continue
		}
		if got.Scheme == "http" && got.Hostname() == want.Hostname() && got.Path == want.Path && got.RawQuery == want.RawQuery {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// optionalHTTPSURL returns raw if it is a valid HTTPS URL, otherwise "". Used
// for display-only links and logos, which must never be javascript: or http.
func optionalHTTPSURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || len(raw) > maxURILength {
		return ""
	}
	return u.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
