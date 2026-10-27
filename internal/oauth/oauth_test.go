package oauth

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
)

func TestValidateRedirectURIs(t *testing.T) {
	ok := [][]string{
		{"https://claude.ai/api/mcp/auth_callback"},
		{"http://localhost:33418/callback"},
		{"http://127.0.0.1/callback", "https://chatgpt.com/connector_platform_oauth_redirect"},
	}
	for _, uris := range ok {
		assert.NoError(t, validateRedirectURIs(uris), uris)
	}
	bad := [][]string{
		nil,
		{"http://example.com/cb"},
		{"https://example.com/cb#frag"},
		{"javascript:alert(1)"},
		{"https://user:pass@example.com/cb"},
		{"/relative"},
	}
	for _, uris := range bad {
		assert.Error(t, validateRedirectURIs(uris), uris)
	}
}

func TestRedirectURIAllowed(t *testing.T) {
	client := &models.OAuthClient{RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback", "http://localhost:5000/callback"}}

	assert.True(t, RedirectURIAllowed(client, "https://claude.ai/api/mcp/auth_callback"))
	assert.True(t, RedirectURIAllowed(client, "http://localhost:61234/callback"), "loopback port may vary")
	assert.False(t, RedirectURIAllowed(client, "https://claude.ai/api/mcp/auth_callback/x"))
	assert.False(t, RedirectURIAllowed(client, "http://localhost:61234/other"))
	assert.False(t, RedirectURIAllowed(client, "https://evil.example/api/mcp/auth_callback"))
}

func TestParseScopes(t *testing.T) {
	all, err := ParseScopes("")
	assert.NoError(t, err)
	assert.Equal(t, AllScopes, all)

	got, err := ParseScopes("campaigns:read messages:read campaigns:read")
	assert.NoError(t, err)
	assert.Equal(t, []Scope{ScopeCampaignsRead, ScopeMessagesRead}, got)

	_, err = ParseScopes("messages:read admin")
	assert.Error(t, err)

	assert.Equal(t, []Scope{ScopeMessagesRead, ScopeCampaignsRead},
		Intersect([]Scope{ScopeCampaignsRead, ScopeMessagesRead, ScopeWebhooksRead}, []Scope{ScopeMessagesRead, ScopeCampaignsRead}))
}

func TestNormalizeResource(t *testing.T) {
	assert.Equal(t, "https://wa.example.com/mcp", NormalizeResource("HTTPS://WA.Example.com/mcp/"))
	assert.Equal(t, "https://wa.example.com", NormalizeResource("https://wa.example.com/"))
}

func TestVerifyPKCE(t *testing.T) {
	// RFC 7636 Appendix B
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	assert.True(t, verifyPKCE(verifier, challenge))
	assert.True(t, ValidChallenge(challenge))
	assert.False(t, verifyPKCE(verifier+"x", challenge))
	assert.False(t, verifyPKCE("short", challenge))
}
