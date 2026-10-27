package mcpserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"

	"github.com/shridarpatil/whatomate/internal/oauth"
	"github.com/valyala/fasthttp"
)

// OAuth authorization server endpoints.
const (
	AuthorizationServerMetadataPath = "/.well-known/oauth-authorization-server"
	AuthorizePath                   = "/oauth/authorize"
	TokenPath                       = "/oauth/token"
	RegisterPath                    = "/oauth/register"
	RevokePath                      = "/oauth/revoke"
	// ConsentPath is the frontend page where users approve a client.
	ConsentPath = "/oauth/consent"
)

// AuthorizationServerMetadataHandler serves RFC 8414 metadata.
func (s *Server) AuthorizationServerMetadataHandler(ctx *fasthttp.RequestCtx) {
	base := s.baseURLCtx(ctx)
	writeJSON(ctx, fasthttp.StatusOK, map[string]any{
		"issuer":                                         base,
		"authorization_endpoint":                         base + AuthorizePath,
		"token_endpoint":                                 base + TokenPath,
		"registration_endpoint":                          base + RegisterPath,
		"revocation_endpoint":                            base + RevokePath,
		"scopes_supported":                               scopeNames(allScopes),
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

// RegisterHandler registers a client (RFC 7591).
func (s *Server) RegisterHandler(ctx *fasthttp.RequestCtx) {
	var md oauth.ClientMetadata
	if err := json.Unmarshal(ctx.PostBody(), &md); err != nil {
		writeOAuthError(ctx, &oauth.Error{Code: oauth.ErrInvalidClientMeta, Description: "request body must be JSON client metadata"})
		return
	}
	client, err := s.oauth.RegisterClient(md)
	if err != nil {
		s.oauthFailure(ctx, err)
		return
	}
	writeJSON(ctx, fasthttp.StatusCreated, map[string]any{
		"client_id":                  client.ClientID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"client_name":                client.Name,
		"client_uri":                 client.URI,
		"logo_uri":                   client.LogoURI,
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// AuthorizeHandler validates an authorization request and sends the browser
// to the consent page. Until the redirect URI is known to belong to the
// client, errors are shown here rather than redirected (RFC 6749 §4.1.2.1).
func (s *Server) AuthorizeHandler(ctx *fasthttp.RequestCtx) {
	args := ctx.QueryArgs()
	param := func(k string) string { return string(args.Peek(k)) }
	base := s.baseURLCtx(ctx)

	clientID := param("client_id")
	if clientID == "" {
		writeErrorPage(ctx, "The request has no client_id.")
		return
	}
	client, err := s.oauth.Client(ctx, clientID)
	if err != nil {
		var oe *oauth.Error
		if errors.As(err, &oe) {
			writeErrorPage(ctx, "This app is not registered: "+oe.Description+".")
			return
		}
		s.app.Log.Error("OAuth client lookup failed", "error", err)
		writeErrorPage(ctx, "Something went wrong. Try connecting again.")
		return
	}
	redirectURI := param("redirect_uri")
	if redirectURI == "" && len(client.RedirectURIs) == 1 {
		redirectURI = client.RedirectURIs[0]
	}
	if !oauth.RedirectURIAllowed(client, redirectURI) {
		writeErrorPage(ctx, "The app asked to return to an address it did not register.")
		return
	}

	state := param("state")
	fail := func(code, description string) {
		ctx.Redirect(oauth.RedirectError(redirectURI, state, base, &oauth.Error{Code: code, Description: description}), fasthttp.StatusFound)
	}
	if param("response_type") != "code" {
		fail(oauth.ErrUnsupportedResponse, "response_type must be code")
		return
	}
	challenge := param("code_challenge")
	if param("code_challenge_method") != "S256" || !oauth.ValidChallenge(challenge) {
		fail(oauth.ErrInvalidRequest, "PKCE with code_challenge_method S256 is required")
		return
	}
	scopes, err := oauth.ParseScopes(param("scope"))
	if err != nil {
		fail(oauth.ErrInvalidScope, "unknown scope; supported scopes are "+strings.Join(scopeNames(allScopes), " "))
		return
	}
	resource := oauth.NormalizeResource(base + Path)
	if r := param("resource"); r != "" && oauth.NormalizeResource(r) != resource {
		fail("invalid_target", "resource must be "+resource)
		return
	}

	id, err := s.oauth.SaveRequest(ctx, &oauth.AuthorizationRequest{
		ClientID:      client.ClientID,
		ClientName:    client.Name,
		ClientURI:     client.URI,
		LogoURI:       client.LogoURI,
		RedirectURI:   redirectURI,
		State:         state,
		CodeChallenge: challenge,
		Scopes:        scopes,
		Resource:      resource,
		Issuer:        base,
	})
	if err != nil {
		s.app.Log.Error("Saving OAuth request failed", "error", err)
		fail("server_error", "could not start authorization")
		return
	}
	ctx.Redirect(base+ConsentPath+"?request="+url.QueryEscape(id), fasthttp.StatusFound)
}

// TokenHandler issues tokens for authorization codes and refresh tokens.
func (s *Server) TokenHandler(ctx *fasthttp.RequestCtx) {
	args := ctx.PostArgs()
	param := func(k string) string { return string(args.Peek(k)) }
	clientID := param("client_id")
	if clientID == "" {
		clientID = basicAuthClientID(ctx)
	}

	var (
		resp *oauth.TokenResponse
		err  error
	)
	switch param("grant_type") {
	case "authorization_code":
		resp, err = s.oauth.ExchangeCode(ctx, param("code"), clientID, param("redirect_uri"), param("code_verifier"), param("resource"))
	case "refresh_token":
		resp, err = s.oauth.Refresh(param("refresh_token"), clientID, param("scope"))
	default:
		err = &oauth.Error{Code: oauth.ErrUnsupportedGrantType, Description: "grant_type must be authorization_code or refresh_token"}
	}
	if err != nil {
		s.oauthFailure(ctx, err)
		return
	}
	writeJSON(ctx, fasthttp.StatusOK, resp)
}

// RevokeHandler revokes a token's connection (RFC 7009). It always succeeds.
func (s *Server) RevokeHandler(ctx *fasthttp.RequestCtx) {
	args := ctx.PostArgs()
	clientID := string(args.Peek("client_id"))
	if clientID == "" {
		clientID = basicAuthClientID(ctx)
	}
	if token := string(args.Peek("token")); token != "" {
		s.oauth.RevokeToken(token, clientID)
	}
	ctx.SetStatusCode(fasthttp.StatusOK)
}

// basicAuthClientID reads client_id from HTTP Basic authentication, which
// some clients use even without a secret.
func basicAuthClientID(ctx *fasthttp.RequestCtx) string {
	h := string(ctx.Request.Header.Peek("Authorization"))
	enc, ok := strings.CutPrefix(h, "Basic ")
	if !ok {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return ""
	}
	id, _, _ := strings.Cut(string(raw), ":")
	id, _ = url.QueryUnescape(id)
	return id
}

// oauthFailure writes an OAuth error, or a server error for anything else.
func (s *Server) oauthFailure(ctx *fasthttp.RequestCtx, err error) {
	var oe *oauth.Error
	if errors.As(err, &oe) {
		writeOAuthError(ctx, oe)
		return
	}
	s.app.Log.Error("OAuth request failed", "path", string(ctx.Path()), "error", err)
	writeOAuthError(ctx, &oauth.Error{Code: "server_error", Description: "internal error"})
}

func writeOAuthError(ctx *fasthttp.RequestCtx, e *oauth.Error) {
	status := fasthttp.StatusBadRequest
	switch e.Code {
	case oauth.ErrInvalidClient:
		status = fasthttp.StatusUnauthorized
	case "server_error":
		status = fasthttp.StatusInternalServerError
	}
	writeJSON(ctx, status, e)
}

func writeJSON(ctx *fasthttp.RequestCtx, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Response.Header.Set("Pragma", "no-cache")
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(status)
	ctx.SetBody(b)
}

// writeErrorPage shows an authorization error to the user in the browser.
func writeErrorPage(ctx *fasthttp.RequestCtx, message string) {
	ctx.SetContentType("text/html; charset=utf-8")
	ctx.SetStatusCode(fasthttp.StatusBadRequest)
	ctx.SetBodyString(fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Cannot connect</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#222}h1{font-size:1.25rem}</style></head>
<body><h1>This app cannot connect to Whatomate</h1><p>%s</p><p>Close this window and try connecting again from your AI assistant.</p></body></html>`, html.EscapeString(message)))
}

// baseURLCtx is baseURL for a fasthttp request.
func (s *Server) baseURLCtx(ctx *fasthttp.RequestCtx) string {
	return s.origin(string(ctx.Host()), string(ctx.Request.Header.Peek("X-Forwarded-Proto")),
		string(ctx.Request.Header.Peek("X-Forwarded-Host")), ctx.IsTLS())
}
