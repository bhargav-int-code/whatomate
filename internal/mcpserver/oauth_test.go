package mcpserver_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/shridarpatil/whatomate/internal/mcpserver"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/oauth"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBase     = "http://whatomate.test"
	testIssuer   = "https://whatomate.test" // derived: not a localhost Host header
	testRedirect = "https://claude.ai/api/mcp/auth_callback"
)

// noRedirects returns e's client without following redirects.
func (e *env) noRedirects() *http.Client {
	return &http.Client{
		Transport:     e.http.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// userToken is a login access token for e's user, as the SPA would hold.
func (e *env) userToken(t *testing.T) string {
	t.Helper()
	claims := middleware.JWTClaims{
		UserID:         e.user.ID,
		OrganizationID: e.org.ID,
		Email:          e.user.Email,
		RoleID:         e.user.RoleID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testutil.TestJWTSecret))
	require.NoError(t, err)
	return token
}

// apiCall calls a logged-in REST route and decodes the envelope's data.
func (e *env) apiCall(t *testing.T, method, path string, body any, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, testBase+path, r)
	req.Header.Set("Authorization", "Bearer "+e.userToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	var env struct {
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if out != nil && len(env.Data) > 0 {
		require.NoError(t, json.Unmarshal(env.Data, out))
	}
	return resp.StatusCode
}

func registerClient(t *testing.T, e *env, redirectURIs ...string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"client_name": "Claude", "redirect_uris": redirectURIs, "token_endpoint_auth_method": "none"})
	resp, err := e.http.Post(testBase+mcpserver.RegisterPath, "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var out struct {
		ClientID string `json:"client_id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.ClientID
}

func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// startAuthorization runs /oauth/authorize and returns the pending request ID.
func startAuthorization(t *testing.T, e *env, clientID, redirectURI, scope, challenge string) string {
	t.Helper()
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"xyz"},
		"scope": {scope}, "resource": {testIssuer + "/mcp"},
	}
	resp, err := e.noRedirects().Get(testBase + mcpserver.AuthorizePath + "?" + q.Encode())
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	require.Equal(t, testIssuer+mcpserver.ConsentPath, loc.Scheme+"://"+loc.Host+loc.Path, "redirects to the consent page")
	return loc.Query().Get("request")
}

// approve approves a request as e's user and returns the authorization code.
func approve(t *testing.T, e *env, requestID string, scopes ...string) string {
	t.Helper()
	var out struct {
		RedirectURL string `json:"redirect_url"`
	}
	status := e.apiCall(t, http.MethodPost, "/api/oauth/requests/"+requestID+"/approve", map[string]any{"scopes": scopes}, &out)
	require.Equal(t, http.StatusOK, status)
	u, err := url.Parse(out.RedirectURL)
	require.NoError(t, err)
	assert.Equal(t, "xyz", u.Query().Get("state"))
	assert.Equal(t, testIssuer, u.Query().Get("iss"))
	return u.Query().Get("code")
}

// tokenRequest posts to the token endpoint and returns the status and body.
func tokenRequest(t *testing.T, e *env, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := e.http.PostForm(testBase+mcpserver.TokenPath, form)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return resp.StatusCode, body
}

// connectWithOAuth runs the whole flow and returns the token response.
func connectWithOAuth(t *testing.T, e *env, scopes ...string) (clientID string, tokens map[string]any) {
	t.Helper()
	clientID = registerClient(t, e, testRedirect)
	verifier, challenge := pkce()
	reqID := startAuthorization(t, e, clientID, testRedirect, "", challenge)
	code := approve(t, e, reqID, scopes...)
	status, tokens := tokenRequest(t, e, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {verifier}, "resource": {testIssuer + "/mcp"},
	})
	require.Equal(t, http.StatusOK, status, tokens)
	return clientID, tokens
}

func TestOAuth_DiscoveryMetadata(t *testing.T) {
	e := newEnv(t)

	resp, err := e.http.Get(testBase + mcpserver.ResourceMetadataPath)
	require.NoError(t, err)
	var prm map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&prm))
	_ = resp.Body.Close()
	assert.Equal(t, []any{testIssuer}, prm["authorization_servers"])

	resp, err = e.http.Get(testBase + mcpserver.AuthorizationServerMetadataPath)
	require.NoError(t, err)
	var asm map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&asm))
	_ = resp.Body.Close()
	assert.Equal(t, testIssuer, asm["issuer"])
	assert.Equal(t, testIssuer+"/oauth/token", asm["token_endpoint"])
	assert.Equal(t, []any{"S256"}, asm["code_challenge_methods_supported"])
	assert.Equal(t, []any{"none"}, asm["token_endpoint_auth_methods_supported"])
	assert.Equal(t, true, asm["client_id_metadata_document_supported"])
}

func TestOAuth_FullFlowGrantsChosenScopes(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)

	_, tokens := connectWithOAuth(t, e, "messages:read")
	assert.Equal(t, "Bearer", tokens["token_type"])
	assert.Equal(t, "messages:read", tokens["scope"])
	assert.EqualValues(t, 3600, tokens["expires_in"])

	cs, err := e.connect(t, tokens["access_token"].(string))
	require.NoError(t, err)
	list, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	assert.Len(t, list.Tools, 10, "only messages:read tools")
	for _, tool := range list.Tools {
		assert.NotContains(t, tool.Name, "send_", tool.Name)
		assert.NotContains(t, tool.Name, "campaign", tool.Name)
	}

	res, text := callTool(t, cs, "whatomate_get_account_info", nil)
	require.False(t, res.IsError, text)
	assert.Contains(t, text, e.user.Email)
	assert.Contains(t, text, "Scopes: messages:read\n")

	var conns struct {
		Connections []map[string]any `json:"connections"`
	}
	require.Equal(t, http.StatusOK, e.apiCall(t, http.MethodGet, "/api/me/connected-apps", nil, &conns))
	require.Len(t, conns.Connections, 1)
	assert.Equal(t, "Claude", conns.Connections[0]["client_name"])

	testutil.AssertEventually(t, func() bool {
		var n int64
		e.app.DB.Model(&models.AuditLog{}).Where("organization_id = ? AND resource_type = ?", e.org.ID, oauth.AuditResource).Count(&n)
		return n == 1
	}, 2*time.Second, "connecting is audited")
}

func TestOAuth_RejectsWrongVerifier(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	clientID := registerClient(t, e, testRedirect)
	_, challenge := pkce()
	code := approve(t, e, startAuthorization(t, e, clientID, testRedirect, "", challenge), "messages:read")
	otherVerifier, _ := pkce()

	status, body := tokenRequest(t, e, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {otherVerifier},
	})
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_grant", body["error"])
}

func TestOAuth_CodeReuseRevokesConnection(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	clientID := registerClient(t, e, testRedirect)
	verifier, challenge := pkce()
	code := approve(t, e, startAuthorization(t, e, clientID, testRedirect, "", challenge), "messages:read")
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {verifier},
	}
	status, tokens := tokenRequest(t, e, form)
	require.Equal(t, http.StatusOK, status)

	status, body := tokenRequest(t, e, form)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_grant", body["error"])

	_, err := e.connect(t, tokens["access_token"].(string))
	assert.Error(t, err, "tokens from a replayed code are revoked")
}

func TestOAuth_RefreshRotatesAndReuseRevokes(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	clientID, tokens := connectWithOAuth(t, e, "messages:read", "campaigns:read")
	oldRefresh := tokens["refresh_token"].(string)

	status, fresh := tokenRequest(t, e, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {oldRefresh}, "client_id": {clientID}})
	require.Equal(t, http.StatusOK, status, fresh)
	assert.NotEqual(t, oldRefresh, fresh["refresh_token"])
	assert.Equal(t, "messages:read campaigns:read", fresh["scope"])
	_, err := e.connect(t, fresh["access_token"].(string))
	require.NoError(t, err)

	// Reusing the old refresh token after the grace period means it leaked
	require.NoError(t, e.app.DB.Model(&models.OAuthToken{}).Where("rotated_at IS NOT NULL").
		Update("rotated_at", time.Now().Add(-time.Minute)).Error)
	status, body := tokenRequest(t, e, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {oldRefresh}, "client_id": {clientID}})
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_grant", body["error"])

	_, err = e.connect(t, fresh["access_token"].(string))
	assert.Error(t, err, "the whole connection is revoked")
}

func TestOAuth_UnregisteredRedirectIsNotFollowed(t *testing.T) {
	e := newEnv(t)
	clientID := registerClient(t, e, testRedirect)
	_, challenge := pkce()
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://evil.example/cb"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	resp, err := e.noRedirects().Get(testBase + mcpserver.AuthorizePath + "?" + q.Encode())
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Location"))
}

func TestOAuth_RequiresPKCE(t *testing.T) {
	e := newEnv(t)
	clientID := registerClient(t, e, testRedirect)
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "state": {"s"}}
	resp, err := e.noRedirects().Get(testBase + mcpserver.AuthorizePath + "?" + q.Encode())
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	assert.True(t, strings.HasPrefix(loc.String(), testRedirect))
	assert.Equal(t, "invalid_request", loc.Query().Get("error"))
	assert.Equal(t, "s", loc.Query().Get("state"))
}

func TestOAuth_RegistrationRejectsInsecureRedirects(t *testing.T) {
	e := newEnv(t)
	b, _ := json.Marshal(map[string]any{"redirect_uris": []string{"http://example.com/cb"}})
	resp, err := e.http.Post(testBase+mcpserver.RegisterPath, "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOAuth_ApproveFailsWhenMCPDisabled(t *testing.T) {
	e := newEnv(t)
	clientID := registerClient(t, e, testRedirect)
	_, challenge := pkce()
	reqID := startAuthorization(t, e, clientID, testRedirect, "", challenge)

	status := e.apiCall(t, http.MethodPost, "/api/oauth/requests/"+reqID+"/approve", map[string]any{"scopes": []string{"messages:read"}}, nil)
	assert.Equal(t, http.StatusForbidden, status)

	// The request survives, so the user can approve once an admin turns MCP on
	e.enableMCP(t)
	assert.NotEmpty(t, approve(t, e, reqID, "messages:read"))
}

func TestOAuth_DenyReturnsAccessDenied(t *testing.T) {
	e := newEnv(t)
	clientID := registerClient(t, e, testRedirect)
	_, challenge := pkce()
	reqID := startAuthorization(t, e, clientID, testRedirect, "", challenge)

	var out struct {
		RedirectURL string `json:"redirect_url"`
	}
	require.Equal(t, http.StatusOK, e.apiCall(t, http.MethodPost, "/api/oauth/requests/"+reqID+"/deny", nil, &out))
	u, _ := url.Parse(out.RedirectURL)
	assert.Equal(t, "access_denied", u.Query().Get("error"))
	assert.Equal(t, http.StatusNotFound, e.apiCall(t, http.MethodGet, "/api/oauth/requests/"+reqID, nil, nil), "answered once")
}

func TestOAuth_RevokedAppLosesAccess(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	_, tokens := connectWithOAuth(t, e, "messages:read")
	var conns struct {
		Connections []struct {
			ID string `json:"id"`
		} `json:"connections"`
	}
	require.Equal(t, http.StatusOK, e.apiCall(t, http.MethodGet, "/api/mcp/connections", nil, &conns))
	require.Len(t, conns.Connections, 1)

	require.Equal(t, http.StatusOK, e.apiCall(t, http.MethodDelete, "/api/me/connected-apps/"+conns.Connections[0].ID, nil, nil))
	_, err := e.connect(t, tokens["access_token"].(string))
	assert.Error(t, err)
}

func TestOAuth_DeactivatedUserLosesAccess(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	_, tokens := connectWithOAuth(t, e, "messages:read")
	require.NoError(t, e.app.DB.Model(e.user).Update("is_active", false).Error)

	_, err := e.connect(t, tokens["access_token"].(string))
	assert.Error(t, err)
}

func TestOAuth_MetadataDocumentClient(t *testing.T) {
	var docURL string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id": docURL, "client_name": "ChatGPT", "redirect_uris": []string{testRedirect},
			"token_endpoint_auth_method": "private_key_jwt",
		})
	}))
	defer ts.Close()
	docURL = ts.URL + "/oauth/client.json"

	e := newEnvWithClient(t, ts.Client())
	_, challenge := pkce()
	reqID := startAuthorization(t, e, docURL, testRedirect, "", challenge)

	var req struct {
		ClientName string   `json:"client_name"`
		Scopes     []string `json:"scopes"`
	}
	require.Equal(t, http.StatusOK, e.apiCall(t, http.MethodGet, "/api/oauth/requests/"+reqID, nil, &req))
	assert.Equal(t, "ChatGPT", req.ClientName)
	assert.Len(t, req.Scopes, 8, "no scope parameter asks for all scopes")
}

func TestOAuth_TokenForAnotherServerIsRejected(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	_, tokens := connectWithOAuth(t, e, "messages:read")
	require.NoError(t, e.app.DB.Model(&models.OAuthConnection{}).Where("organization_id = ?", e.org.ID).
		Update("resource", "https://other.example/mcp").Error)

	_, err := e.connect(t, tokens["access_token"].(string))
	assert.Error(t, err)
}
