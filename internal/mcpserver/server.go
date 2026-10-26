// Package mcpserver exposes Whatomate to AI assistants (Claude, ChatGPT, ...)
// over the Model Context Protocol.
//
// Every tool is a thin wrapper over the REST API: it calls the same routes the
// frontend uses, in-process and as the connected user, so permissions, rate
// limits, validation and audit behave exactly as they do for that user in the
// UI. Scopes granted to the connection narrow that further.
package mcpserver

import (
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

// Path is where the MCP endpoint is mounted.
const Path = "/mcp"

// ResourceMetadataPath serves the RFC 9728 protected resource metadata for
// Path, which tells clients how to get a token.
const ResourceMetadataPath = "/.well-known/oauth-protected-resource" + Path

const serverInstructions = `Whatomate is a WhatsApp Business platform. Use these tools to look up contacts and conversations, send WhatsApp messages, run template campaigns and inspect message and webhook delivery logs.

WhatsApp rules to keep in mind:
- Free-form text can only be sent within 24 hours of the contact's last message (the "service window"). Outside it, send an approved template instead.
- Campaigns send one approved template to many recipients. Create the campaign, add recipients, then start it; whatomate_start_campaign first returns a preview and only sends when called again with confirm=true.
- Phone numbers are in international format without "+" or spaces, e.g. 919876543210.

Everything you do is attributed to the connected user and limited by their role.`

// Server serves the MCP endpoint.
type Server struct {
	app       *handlers.App
	publicURL string
	dispatch  *dispatcher
	handler   fasthttp.RequestHandler

	// servers caches one MCP server per distinct scope set, so each
	// connection only sees the tools its scopes allow.
	mu      sync.Mutex
	servers map[string]*mcp.Server
}

// New creates the MCP server. router must be the application's full fastglue
// handler (including its auth middleware): tools call REST routes through it.
func New(app *handlers.App, router fasthttp.RequestHandler) *Server {
	s := &Server{
		app:       app,
		publicURL: strings.TrimRight(app.Config.Server.PublicURL, "/"),
		dispatch:  newDispatcher(router, app.Config.JWT.Secret),
		servers:   make(map[string]*mcp.Server),
	}

	streamable := mcp.NewStreamableHTTPHandler(s.serverFor, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// Requests are authenticated with bearer tokens, which a DNS
		// rebinding page cannot attach, and behind a reverse proxy or ngrok
		// a loopback peer with a public Host header is the normal case.
		DisableLocalhostProtection:   true,
		PropagateRequestCancellation: true,
	})

	var h http.Handler = streamable
	h = s.requireEnabled(h)
	// The 401 challenge's resource_metadata is added by withResourceMetadata,
	// since the public URL may come from the request's Host header.
	h = auth.RequireBearerToken(s.verifyToken, &auth.RequireBearerTokenOptions{
		AllowMissingExpiration: true,
	})(h)
	h = s.withResourceMetadata(h)
	h = apiKeyHeaderAsBearer(h)
	s.handler = fasthttpadaptor.NewFastHTTPHandler(h)
	return s
}

// Handler serves Path.
func (s *Server) Handler(ctx *fasthttp.RequestCtx) {
	s.handler(ctx)
}

// ResourceMetadataHandler serves ResourceMetadataPath.
func (s *Server) ResourceMetadataHandler(ctx *fasthttp.RequestCtx) {
	fasthttpadaptor.NewFastHTTPHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := s.baseURL(r)
		auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               base + Path,
			ResourceName:           "Whatomate",
			BearerMethodsSupported: []string{"header"},
			ScopesSupported:        scopeNames(allScopes),
			// No authorization server yet: until OAuth support lands,
			// clients connect with an API key as the bearer token.
		}).ServeHTTP(w, r)
	})(ctx)
}

// baseURL is the server's public origin: the configured public URL, or one
// derived from the request.
func (s *Server) baseURL(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS == nil && isLocalHost(r.Host) {
		scheme = "http"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host
}

func isLocalHost(host string) bool {
	h := host
	if i := strings.LastIndex(h, ":"); i != -1 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "[::1]"
}

// withResourceMetadata points 401 responses at this request's metadata URL.
func (s *Server) withResourceMetadata(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&resourceMetadataWriter{ResponseWriter: w, url: s.baseURL(r) + ResourceMetadataPath}, r)
	})
}

// resourceMetadataWriter adds resource_metadata to the WWW-Authenticate
// challenge of a 401, which RFC 9728 clients use to discover authorization.
type resourceMetadataWriter struct {
	http.ResponseWriter
	url string
}

func (w *resourceMetadataWriter) WriteHeader(code int) {
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+w.url+`"`)
	}
	w.ResponseWriter.WriteHeader(code)
}

// serverFor returns the MCP server exposing the tools the request's scopes
// allow.
func (s *Server) serverFor(r *http.Request) *mcp.Server {
	p := principalFrom(auth.TokenInfoFromContext(r.Context()))
	if p == nil {
		return nil
	}
	scopes := slices.Clone(p.Scopes)
	slices.Sort(scopes)
	key := strings.Join(scopeNames(scopes), " ")

	s.mu.Lock()
	defer s.mu.Unlock()
	if srv, ok := s.servers[key]; ok {
		return srv
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "whatomate",
		Title:   "Whatomate",
		Version: "1.0.0",
	}, &mcp.ServerOptions{Instructions: serverInstructions})
	s.registerTools(srv, scopes)
	s.servers[key] = srv
	return srv
}
