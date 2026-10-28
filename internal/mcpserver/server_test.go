package mcpserver_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/mcpserver"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
	"github.com/zerodha/fastglue"
	"golang.org/x/crypto/bcrypt"
)

// env is a running Whatomate API with the MCP endpoint, reachable through
// an in-memory listener.
type env struct {
	meta   *fakeMeta
	app    *handlers.App
	org    *models.Organization
	user   *models.User
	apiKey string
	http   *http.Client
}

func newEnv(t *testing.T, permissionKeys ...string) *env {
	t.Helper()
	return newEnvWithClient(t, &http.Client{Timeout: 5 * time.Second}, permissionKeys...)
}

// newEnvWithClient is newEnv with the HTTP client used for outbound requests,
// such as fetching client metadata documents.
func newEnvWithClient(t *testing.T, httpClient *http.Client, permissionKeys ...string) *env {
	t.Helper()
	db := testutil.SetupTestDB(t)
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set, skipping test")
	}
	app := &handlers.App{
		Config: &config.Config{
			App: config.AppConfig{EncryptionKey: "test-encryption-key-for-handlers-longer-than-32-chars"},
			JWT: config.JWTConfig{Secret: testutil.TestJWTSecret, AccessExpiryMins: 15, RefreshExpiryDays: 7},
		},
		DB:         db,
		Log:        testutil.NopLogger(),
		Redis:      rdb,
		HTTPClient: httpClient,
	}
	meta := newFakeMeta(t)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, meta.server.URL)

	org := testutil.CreateTestOrganization(t, db)
	var role *models.CustomRole
	if len(permissionKeys) == 0 {
		role = testutil.CreateAdminRole(t, db, org.ID)
	} else {
		role = testutil.CreateTestRoleWithKeys(t, db, org.ID, "limited", permissionKeys)
	}
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	testutil.CreateTestWhatsAppAccount(t, db, org.ID)

	e := &env{meta: meta, app: app, org: org, user: user, apiKey: createAPIKey(t, app, org.ID, user.ID)}

	g := fastglue.NewGlue()
	g.Before(func(r *fastglue.Request) *fastglue.Request {
		if strings.HasPrefix(string(r.RequestCtx.Path()), "/api") {
			return middleware.AuthWithDB(app.Config.JWT.Secret, app.DB)(r)
		}
		return r
	})
	registerRoutes(g, app)
	srv := mcpserver.New(app, g.Handler())
	g.POST(mcpserver.Path, func(r *fastglue.Request) error { srv.Handler(r.RequestCtx); return nil })
	g.GET(mcpserver.ResourceMetadataPath, func(r *fastglue.Request) error { srv.ResourceMetadataHandler(r.RequestCtx); return nil })
	fast := func(h fasthttp.RequestHandler) fastglue.FastRequestHandler {
		return func(r *fastglue.Request) error { h(r.RequestCtx); return nil }
	}
	g.GET(mcpserver.AuthorizationServerMetadataPath, fast(srv.AuthorizationServerMetadataHandler))
	g.POST(mcpserver.RegisterPath, fast(srv.RegisterHandler))
	g.GET(mcpserver.AuthorizePath, fast(srv.AuthorizeHandler))
	g.POST(mcpserver.TokenPath, fast(srv.TokenHandler))
	g.POST(mcpserver.RevokePath, fast(srv.RevokeHandler))

	ln := fasthttputil.NewInmemoryListener()
	server := &fasthttp.Server{Handler: g.Handler()}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	e.http = &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return ln.Dial() },
	}}
	return e
}

// registerRoutes mounts the REST routes the MCP tools call, as main.go does.
func registerRoutes(g *fastglue.Fastglue, app *handlers.App) {
	g.GET("/api/me", app.GetCurrentUser)
	g.GET("/api/organizations/current", app.GetCurrentOrganization)
	g.GET("/api/accounts", app.ListAccounts)
	g.GET("/api/contacts", app.ListContacts)
	g.POST("/api/contacts", app.CreateContact)
	g.GET("/api/contacts/{id}", app.GetContact)
	g.PUT("/api/contacts/{id}/tags", app.UpdateContactTags)
	g.GET("/api/contacts/{id}/messages", app.GetMessages)
	g.POST("/api/contacts/{id}/messages", app.SendMessage)
	g.POST("/api/messages/template", app.SendTemplateMessage)
	g.GET("/api/templates", app.ListTemplates)
	g.POST("/api/templates", app.CreateTemplate)
	g.POST("/api/templates/sync", app.SyncTemplates)
	g.POST("/api/templates/upload-media", app.UploadTemplateMedia)
	g.GET("/api/templates/{id}", app.GetTemplate)
	g.PUT("/api/templates/{id}", app.UpdateTemplate)
	g.POST("/api/templates/{id}/publish", app.SubmitTemplate)
	g.GET("/api/flows", app.ListFlows)
	g.POST("/api/flows", app.CreateFlow)
	g.GET("/api/flows/{id}", app.GetFlow)
	g.PUT("/api/flows/{id}", app.UpdateFlow)
	g.POST("/api/flows/{id}/save-to-meta", app.SaveFlowToMeta)
	g.POST("/api/flows/{id}/publish", app.PublishFlow)
	g.POST("/api/flows/{id}/duplicate", app.DuplicateFlow)
	g.GET("/api/message-logs", app.ListMessageLogs)
	g.GET("/api/message-logs/{id}", app.GetMessageLog)
	g.GET("/api/analytics/dashboard", app.GetDashboardStats)
	g.GET("/api/campaigns", app.ListCampaigns)
	g.POST("/api/campaigns", app.CreateCampaign)
	g.GET("/api/campaigns/{id}", app.GetCampaign)
	g.POST("/api/campaigns/{id}/start", app.StartCampaign)
	g.POST("/api/campaigns/{id}/pause", app.PauseCampaign)
	g.POST("/api/campaigns/{id}/cancel", app.CancelCampaign)
	g.POST("/api/campaigns/{id}/recipients/import", app.ImportRecipients)
	g.GET("/api/campaigns/{id}/recipients", app.GetCampaignRecipients)
	g.GET("/api/webhook-logs", app.ListWebhookLogs)
	g.GET("/api/webhook-logs/{id}", app.GetWebhookLog)
	g.GET("/api/chatbot/settings", app.GetChatbotSettings)
	g.PUT("/api/chatbot/settings", app.UpdateChatbotSettings)
	g.GET("/api/chatbot/keywords", app.ListKeywordRules)
	g.POST("/api/chatbot/keywords", app.CreateKeywordRule)
	g.GET("/api/chatbot/keywords/{id}", app.GetKeywordRule)
	g.PUT("/api/chatbot/keywords/{id}", app.UpdateKeywordRule)
	g.GET("/api/chatbot/flows", app.ListChatbotFlows)
	g.POST("/api/chatbot/flows", app.CreateChatbotFlow)
	g.GET("/api/chatbot/flows/{id}", app.GetChatbotFlow)
	g.PUT("/api/chatbot/flows/{id}", app.UpdateChatbotFlow)
	g.GET("/api/chatbot/ai-contexts", app.ListAIContexts)
	g.POST("/api/chatbot/ai-contexts", app.CreateAIContext)
	g.GET("/api/chatbot/ai-contexts/{id}", app.GetAIContext)
	g.PUT("/api/chatbot/ai-contexts/{id}", app.UpdateAIContext)
	g.GET("/api/webhooks", app.ListWebhooks)
	g.POST("/api/webhooks", app.CreateWebhook)
	g.GET("/api/webhooks/{id}", app.GetWebhook)
	g.PUT("/api/webhooks/{id}", app.UpdateWebhook)
	g.POST("/api/webhooks/{id}/test", app.TestWebhook)
	g.GET("/api/oauth/requests/{id}", app.GetOAuthRequest)
	g.POST("/api/oauth/requests/{id}/approve", app.ApproveOAuthRequest)
	g.POST("/api/oauth/requests/{id}/deny", app.DenyOAuthRequest)
	g.GET("/api/me/connected-apps", app.ListMyConnectedApps)
	g.DELETE("/api/me/connected-apps/{id}", app.RevokeMyConnectedApp)
	g.GET("/api/mcp/connections", app.ListMCPConnections)
	g.DELETE("/api/mcp/connections/{id}", app.RevokeMCPConnection)
}

func createAPIKey(t *testing.T, app *handlers.App, orgID, userID uuid.UUID) string {
	t.Helper()
	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(t, err)
	key := "whm_" + hex.EncodeToString(b)
	hash, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, app.DB.Create(&models.APIKey{
		OrganizationID: orgID, UserID: userID, Name: "Claude test", KeyPrefix: key[4:20], KeyHash: string(hash), IsActive: true,
	}).Error)
	return key
}

func (e *env) enableMCP(t *testing.T) {
	t.Helper()
	require.NoError(t, e.app.DB.Model(e.org).Update("settings", models.JSONB{"mcp_enabled": true}).Error)
}

// bearer adds an Authorization header to every request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// connect opens an MCP client session authenticated with token.
func (e *env) connect(t *testing.T, token string) (*mcp.ClientSession, error) {
	t.Helper()
	hc := &http.Client{Transport: bearer{token: token, next: e.http.Transport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: "http://whatomate.test" + mcpserver.Path, HTTPClient: hc, DisableStandaloneSSE: true,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return res, text.String()
}

func TestMCP_RequiresBearerToken(t *testing.T) {
	e := newEnv(t)
	resp, err := e.http.Post("http://whatomate.test/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, `Bearer resource_metadata="https://whatomate.test/.well-known/oauth-protected-resource/mcp"`,
		resp.Header.Get("WWW-Authenticate"))
}

func TestMCP_RejectsInvalidAPIKey(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	_, err := e.connect(t, "whm_00000000000000000000000000000000")
	require.Error(t, err)
}

func TestMCP_ResourceMetadata(t *testing.T) {
	e := newEnv(t)
	resp, err := e.http.Get("http://whatomate.test" + mcpserver.ResourceMetadataPath)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var meta map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&meta))
	assert.Equal(t, "https://whatomate.test/mcp", meta["resource"])
	assert.Contains(t, meta["scopes_supported"], "messages:send")
}

func TestMCP_DisabledForOrganization(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodPost, "http://whatomate.test/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", e.apiKey)
	resp, err := e.http.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Contains(t, body["error_description"], "turned off")
}

func TestMCP_ListsAllTools(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	cs, err := e.connect(t, e.apiKey)
	require.NoError(t, err)

	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	assert.Len(t, res.Tools, 55)
	for _, tool := range res.Tools {
		assert.True(t, strings.HasPrefix(tool.Name, "whatomate_"), tool.Name)
		assert.NotEmpty(t, tool.Description, tool.Name)
		require.NotNil(t, tool.Annotations, tool.Name)
	}
}

func TestMCP_GetAccountInfo(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	cs, err := e.connect(t, e.apiKey)
	require.NoError(t, err)

	res, text := callTool(t, cs, "whatomate_get_account_info", nil)
	require.False(t, res.IsError, text)
	assert.Contains(t, text, e.user.Email)
	assert.Contains(t, text, e.org.Name)
	assert.Contains(t, text, "campaigns:write")
}

func TestMCP_SearchContacts(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	contact := testutil.CreateTestContact(t, e.app.DB, e.org.ID)
	cs, err := e.connect(t, e.apiKey)
	require.NoError(t, err)

	res, text := callTool(t, cs, "whatomate_search_contacts", map[string]any{"query": contact.ProfileName})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, contact.ID.String())

	structured, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(structured), `"service_window_open":false`)
}

func TestMCP_SendTextOutsideServiceWindowSuggestsTemplate(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	contact := testutil.CreateTestContact(t, e.app.DB, e.org.ID)
	cs, err := e.connect(t, e.apiKey)
	require.NoError(t, err)

	res, text := callTool(t, cs, "whatomate_send_text_message", map[string]any{"contact_id": contact.ID.String(), "text": "hi"})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "whatomate_send_template_message")

	var count int64
	e.app.DB.Model(&models.Message{}).Where("contact_id = ?", contact.ID).Count(&count)
	assert.Zero(t, count, "nothing may be sent")
}

func TestMCP_GetConversationDoesNotMarkRead(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	contact := testutil.CreateTestContact(t, e.app.DB, e.org.ID)
	msg := models.Message{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: e.org.ID,
		ContactID:      contact.ID,
		Direction:      models.DirectionIncoming,
		MessageType:    models.MessageTypeText,
		Content:        "When does the court open?",
		Status:         models.MessageStatusReceived,
	}
	require.NoError(t, e.app.DB.Create(&msg).Error)
	cs, err := e.connect(t, e.apiKey)
	require.NoError(t, err)

	res, text := callTool(t, cs, "whatomate_get_conversation", map[string]any{"contact_id": contact.ID.String()})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "When does the court open?")

	var after models.Message
	require.NoError(t, e.app.DB.First(&after, "id = ?", msg.ID).Error)
	assert.Equal(t, models.MessageStatusReceived, after.Status)
}

func TestMCP_StartCampaignPreviewsUntilConfirmed(t *testing.T) {
	e := newEnv(t)
	e.enableMCP(t)
	var account models.WhatsAppAccount
	require.NoError(t, e.app.DB.Where("organization_id = ?", e.org.ID).First(&account).Error)
	template := testutil.CreateTestTemplate(t, e.app.DB, e.org.ID, account.Name)
	cs, err := e.connect(t, e.apiKey)
	require.NoError(t, err)

	res, text := callTool(t, cs, "whatomate_create_campaign", map[string]any{
		"name": "Diwali offer", "template_id": template.ID.String(), "whatsapp_account": account.Name,
	})
	require.False(t, res.IsError, text)
	var campaign models.BulkMessageCampaign
	require.NoError(t, e.app.DB.Where("organization_id = ? AND name = ?", e.org.ID, "Diwali offer").First(&campaign).Error)

	res, text = callTool(t, cs, "whatomate_add_campaign_recipients", map[string]any{
		"campaign_id": campaign.ID.String(),
		"recipients": []map[string]any{
			{"phone_number": "+919876543210", "template_params": map[string]string{"1": "Asha"}},
			{"phone_number": "919876543211", "template_params": map[string]string{"1": "Ravi"}},
		},
	})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "Added 2 recipients")

	res, text = callTool(t, cs, "whatomate_start_campaign", map[string]any{"campaign_id": campaign.ID.String()})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "PREVIEW")
	assert.Contains(t, text, "2 recipients")

	require.NoError(t, e.app.DB.First(&campaign, "id = ?", campaign.ID).Error)
	assert.Equal(t, models.CampaignStatusDraft, campaign.Status, "a preview must not start the campaign")

	var audits int64
	testutil.AssertEventually(t, func() bool {
		e.app.DB.Model(&models.AuditLog{}).Where("organization_id = ? AND resource_type = ? AND changes::text LIKE ?",
			e.org.ID, models.ResourceCampaigns, "%AI assistant (MCP)%").Count(&audits)
		return audits >= 2
	}, 2*time.Second, "MCP campaign writes are audited")
}

func TestMCP_RolePermissionsStillApply(t *testing.T) {
	e := newEnv(t, "contacts:read", "chat:read")
	e.enableMCP(t)
	cs, err := e.connect(t, e.apiKey)
	require.NoError(t, err)

	// The campaigns REST handlers only require a login, so this is enforced
	// by the MCP layer
	res, text := callTool(t, cs, "whatomate_list_campaigns", nil)
	assert.True(t, res.IsError)
	assert.Contains(t, text, "campaigns:read")

	res, text = callTool(t, cs, "whatomate_search_contacts", nil)
	assert.False(t, res.IsError, text)
}

func TestMCP_StartCampaignNeedsExecutePermission(t *testing.T) {
	e := newEnv(t, "campaigns:read", "campaigns:write")
	e.enableMCP(t)
	cs, err := e.connect(t, e.apiKey)
	require.NoError(t, err)

	res, text := callTool(t, cs, "whatomate_start_campaign", map[string]any{"campaign_id": uuid.New().String(), "confirm": true})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "campaigns:execute")
}

// fakeMeta stands in for Meta's Graph API.
type fakeMeta struct {
	server *httptest.Server
	mu     sync.Mutex
	paths  []string
	// rejectFlow makes flow JSON uploads fail Meta's validation
	rejectFlow bool
}

func newFakeMeta(t *testing.T) *fakeMeta {
	t.Helper()
	m := &fakeMeta{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.paths = append(m.paths, r.Method+" "+r.URL.Path)
		reject := m.rejectFlow
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/uploads"):
			_, _ = io.WriteString(w, `{"id":"upload:abc"}`)
		case strings.HasSuffix(p, "/upload:abc"):
			_, _ = io.WriteString(w, `{"h":"4::aGFuZGxlLWZvci1zYW1wbGUtbWVkaWE="}`)
		case strings.HasSuffix(p, "/message_templates") && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"meta-tmpl-1","status":"PENDING","category":"UTILITY"}`)
		case strings.HasSuffix(p, "/message_templates"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case strings.HasSuffix(p, "/flows") && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"meta-flow-1"}`)
		case strings.HasSuffix(p, "/meta-flow-1/assets") && reject:
			_, _ = io.WriteString(w, `{"success":false,"validation_errors":[{"error":"INVALID_PROPERTY_VALUE","message":"Invalid value for property 'type' at /screens/0/layout/children/0"}]}`)
		case strings.HasSuffix(p, "/meta-flow-1/assets"):
			_, _ = io.WriteString(w, `{"success":true}`)
		case strings.HasSuffix(p, "/meta-flow-1/publish"):
			_, _ = io.WriteString(w, `{"success":true}`)
		case strings.HasSuffix(p, "/meta-flow-1"):
			_, _ = io.WriteString(w, `{"id":"meta-flow-1","name":"Booking","status":"DRAFT","preview":{"preview_url":"https://business.facebook.com/wa/manage/flows/preview/meta-flow-1"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"unexpected call `+r.Method+` `+p+`","code":100}}`)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *fakeMeta) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.paths...)
}
