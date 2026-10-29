package mcpserver_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// structured decodes a tool result's structured content.
func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func connected(t *testing.T, permissionKeys ...string) (*env, *mcp.ClientSession) {
	t.Helper()
	e := newEnv(t, permissionKeys...)
	e.enableMCP(t)
	cs, err := e.connect(t, e.apiKey)
	require.NoError(t, err)
	return e, cs
}

func TestMCP_KeywordRuleCreateAndUpdate(t *testing.T) {
	e, cs := connected(t)

	res, text := callTool(t, cs, "whatomate_create_keyword_rule", map[string]any{
		"name": "Pricing", "keywords": []string{"price", "cost"}, "response_text": "Courts are ₹500/hour.",
		"buttons": []map[string]any{{"id": "book", "title": "Book now"}},
	})
	require.False(t, res.IsError, text)
	id := structured(t, res)["id"].(string)

	var rule models.KeywordRule
	require.NoError(t, e.app.DB.First(&rule, "id = ?", id).Error)
	assert.Equal(t, models.MatchTypeContains, rule.MatchType)
	assert.True(t, rule.IsEnabled)
	assert.Equal(t, "Courts are ₹500/hour.", rule.ResponseContent["body"])

	res, text = callTool(t, cs, "whatomate_update_keyword_rule", map[string]any{"id": id, "enabled": false})
	require.False(t, res.IsError, text)
	require.NoError(t, e.app.DB.First(&rule, "id = ?", id).Error)
	assert.False(t, rule.IsEnabled)
	assert.Equal(t, "Pricing", rule.Name, "fields not passed are kept")

	res, text = callTool(t, cs, "whatomate_create_keyword_rule", map[string]any{
		"name": "Bad", "keywords": []string{"x"}, "response_text": "y",
		"buttons": []map[string]any{{"id": "a", "title": "This title is far too long"}},
	})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "WhatsApp allows 20")
}

func TestMCP_APIContextNeedsConfirmation(t *testing.T) {
	e, cs := connected(t)
	args := map[string]any{"name": "Slots", "context_type": "api", "api_url": "https://booking.example/slots"}

	res, text := callTool(t, cs, "whatomate_create_ai_context", args)
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "PREVIEW")
	assert.Contains(t, text, "https://booking.example/slots")
	var count int64
	e.app.DB.Model(&models.AIContext{}).Where("organization_id = ?", e.org.ID).Count(&count)
	assert.Zero(t, count, "a preview saves nothing")

	args["confirm"] = true
	res, text = callTool(t, cs, "whatomate_create_ai_context", args)
	require.False(t, res.IsError, text)
	e.app.DB.Model(&models.AIContext{}).Where("organization_id = ?", e.org.ID).Count(&count)
	assert.EqualValues(t, 1, count)

	// A static context has no outside URL, so it saves straight away
	res, text = callTool(t, cs, "whatomate_create_ai_context", map[string]any{"name": "FAQ", "static_content": "We open at 6am."})
	require.False(t, res.IsError, text)
	assert.NotContains(t, text, "PREVIEW")
}

func validFlowGraph() map[string]any {
	return map[string]any{
		"nodes": []map[string]any{
			{"id": "start", "type": "start"},
			{"id": "ask", "type": "prompt", "config": map[string]any{"body": "Your name?", "store_as": "name"}},
			{"id": "hi", "type": "message", "config": map[string]any{"message": "Hi {{name}}"}},
		},
		"edges": []map[string]any{{"from": "start", "to": "ask"}, {"from": "ask", "to": "hi"}},
	}
}

func TestMCP_ChatbotFlowCreateValidatesAndStartsDisabled(t *testing.T) {
	e, cs := connected(t)

	broken := validFlowGraph()
	broken["edges"] = []map[string]any{{"from": "start", "to": "missing"}}
	res, text := callTool(t, cs, "whatomate_create_chatbot_flow", map[string]any{"name": "Greet", "trigger_keywords": []string{"hi"}, "graph": broken})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "nothing was saved")

	res, text = callTool(t, cs, "whatomate_create_chatbot_flow", map[string]any{"name": "Greet", "trigger_keywords": []string{"hi"}, "graph": validFlowGraph()})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, `node "hi" (message) has no outgoing edge`, "warnings are reported")
	id := structured(t, res)["id"].(string)

	var flow models.ChatbotFlow
	require.NoError(t, e.app.DB.First(&flow, "id = ?", id).Error)
	assert.False(t, flow.IsEnabled, "new flows start disabled for review")
	assert.EqualValues(t, 2, flow.Graph["version"])
	assert.Equal(t, "start", flow.Graph["entry_node"])

	res, text = callTool(t, cs, "whatomate_get_chatbot_flow", map[string]any{"id": id})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "3 nodes, 2 edges")

	res, text = callTool(t, cs, "whatomate_update_chatbot_flow", map[string]any{"id": id, "enabled": true})
	require.False(t, res.IsError, text)
	require.NoError(t, e.app.DB.First(&flow, "id = ?", id).Error)
	assert.True(t, flow.IsEnabled)
	assert.Equal(t, "Greet", flow.Name)
}

func TestMCP_FlowWithAPICallNeedsConfirmation(t *testing.T) {
	e, cs := connected(t)
	graph := validFlowGraph()
	nodes := graph["nodes"].([]map[string]any)
	graph["nodes"] = append(nodes, map[string]any{"id": "save", "type": "api_call", "config": map[string]any{"url": "https://crm.example/leads", "method": "POST"}})
	graph["edges"] = append(graph["edges"].([]map[string]any), map[string]any{"from": "hi", "to": "save"})
	args := map[string]any{"name": "Lead", "trigger_keywords": []string{"lead"}, "graph": graph}

	res, text := callTool(t, cs, "whatomate_create_chatbot_flow", args)
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "PREVIEW")
	assert.Contains(t, text, "https://crm.example/leads")
	var count int64
	e.app.DB.Model(&models.ChatbotFlow{}).Where("organization_id = ?", e.org.ID).Count(&count)
	assert.Zero(t, count)

	args["confirm"] = true
	res, text = callTool(t, cs, "whatomate_create_chatbot_flow", args)
	require.False(t, res.IsError, text)
	id := structured(t, res)["id"].(string)

	// Changing other nodes while keeping the same URL needs no new confirmation
	nodes = graph["nodes"].([]map[string]any)
	nodes[2]["config"] = map[string]any{"message": "Hello {{name}}"}
	res, text = callTool(t, cs, "whatomate_update_chatbot_flow", map[string]any{"id": id, "graph": graph})
	require.False(t, res.IsError, text)
	assert.NotContains(t, text, "PREVIEW")
}

func TestMCP_ChatbotSettingsCannotTouchAIKey(t *testing.T) {
	e, cs := connected(t)

	res, text := callTool(t, cs, "whatomate_update_chatbot_settings", map[string]any{
		"enabled": true, "greeting_message": "Welcome to Sportomic!",
		"business_hours_enabled": true,
		"business_hours":         []map[string]any{{"day": 1, "enabled": true, "start_time": "06:00", "end_time": "22:00"}},
	})
	require.False(t, res.IsError, text)

	res, text = callTool(t, cs, "whatomate_get_chatbot_settings", nil)
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "Welcome to Sportomic!")

	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whatomate_update_chatbot_settings", Arguments: map[string]any{"ai_api_key": "sk-stolen"}})
	if err == nil {
		var settings models.ChatbotSettings
		require.NoError(t, e.app.DB.Where("organization_id = ?", e.org.ID).First(&settings).Error)
		assert.Empty(t, settings.AI.APIKey, "the AI key is not settable through MCP")
	}
}

func TestMCP_WebhookCreateAndUpdateNeedConfirmation(t *testing.T) {
	e, cs := connected(t)
	args := map[string]any{"name": "CRM", "url": "https://crm.example/hook", "events": []string{"message.incoming", "message.read"}}

	res, text := callTool(t, cs, "whatomate_create_webhook", args)
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "PREVIEW")
	var count int64
	e.app.DB.Model(&models.Webhook{}).Where("organization_id = ?", e.org.ID).Count(&count)
	assert.Zero(t, count)

	res, text = callTool(t, cs, "whatomate_create_webhook", map[string]any{"name": "Bad", "url": "https://x.example", "events": []string{"message.teleported"}})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "unknown event")

	args["confirm"] = true
	args["headers"] = map[string]any{"Authorization": "Bearer secret-token"}
	res, text = callTool(t, cs, "whatomate_create_webhook", args)
	require.False(t, res.IsError, text)
	view := structured(t, res)
	id := view["id"].(string)
	assert.Equal(t, true, view["active"])
	assert.NotContains(t, text, "secret-token", "header values are never shown")

	// A partial update must not switch the webhook off
	res, text = callTool(t, cs, "whatomate_update_webhook", map[string]any{"id": id, "name": "CRM v2"})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "PREVIEW")
	res, text = callTool(t, cs, "whatomate_update_webhook", map[string]any{"id": id, "name": "CRM v2", "confirm": true})
	require.False(t, res.IsError, text)
	var wh models.Webhook
	require.NoError(t, e.app.DB.First(&wh, "id = ?", id).Error)
	assert.Equal(t, "CRM v2", wh.Name)
	assert.True(t, wh.IsActive)
	assert.Equal(t, "https://crm.example/hook", wh.URL)
}

func TestMCP_ChatbotToolsNeedRolePermission(t *testing.T) {
	_, cs := connected(t, "chatbot.keywords:read")

	res, text := callTool(t, cs, "whatomate_list_keyword_rules", nil)
	assert.False(t, res.IsError, text)

	res, text = callTool(t, cs, "whatomate_create_keyword_rule", map[string]any{"name": "x", "keywords": []string{"x"}, "response_text": "y"})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "chatbot.keywords:write")

	res, text = callTool(t, cs, "whatomate_create_webhook", map[string]any{"name": "x", "url": "https://x.example", "events": []string{"message.incoming"}, "confirm": true})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "webhooks:write")
}
