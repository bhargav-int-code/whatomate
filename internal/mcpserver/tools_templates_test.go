package mcpserver_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAccountName returns the name of e's WhatsApp account.
func testAccountName(t *testing.T, e *env) string {
	t.Helper()
	var account models.WhatsAppAccount
	require.NoError(t, e.app.DB.Where("organization_id = ?", e.org.ID).First(&account).Error)
	return account.Name
}

func countCalls(calls []string, suffix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasSuffix(c, suffix) {
			n++
		}
	}
	return n
}

func TestMCP_CreateTemplateValidatesMetaRules(t *testing.T) {
	e, cs := connected(t)
	base := map[string]any{"whatsapp_account": testAccountName(t, e), "name": "order_shipped", "language": "en", "category": "UTILITY",
		"body": "Hi {{1}}, your order {{2}} has shipped.", "body_examples": map[string]any{"1": "Asha", "2": "#4521"}}

	for name, change := range map[string]map[string]any{
		"needs examples":     {"body_examples": map[string]any{"1": "Asha"}},
		"variable at start":  {"body": "{{1}}, your order has shipped.", "body_examples": map[string]any{"1": "Asha"}},
		"bad name":           {"name": "Order Shipped"},
		"long button":        {"buttons": []map[string]any{{"type": "quick_reply", "text": "This label is way too long for Meta"}}},
		"mixed variables":    {"body": "Hi {{1}}, order {{order}} shipped.", "body_examples": map[string]any{"1": "a", "order": "b"}},
		"footer variable":    {"footer": "Ref {{1}}"},
		"missing media url":  {"header_type": "image"},
		"three url buttons":  {"buttons": []map[string]any{{"type": "url", "text": "a", "url": "https://a.example"}, {"type": "url", "text": "b", "url": "https://b.example"}, {"type": "url", "text": "c", "url": "https://c.example"}}},
		"url var no example": {"buttons": []map[string]any{{"type": "url", "text": "Track", "url": "https://t.example/{{1}}"}}},
	} {
		args := map[string]any{}
		for k, v := range base {
			args[k] = v
		}
		for k, v := range change {
			args[k] = v
		}
		res, text := callTool(t, cs, "whatomate_create_template", args)
		assert.True(t, res.IsError, "%s: %s", name, text)
	}

	res, text := callTool(t, cs, "whatomate_create_template", base)
	require.False(t, res.IsError, text)
	var tmpl models.Template
	require.NoError(t, e.app.DB.Where("organization_id = ? AND name = ?", e.org.ID, "order_shipped").First(&tmpl).Error)
	assert.Equal(t, "DRAFT", tmpl.Status)
	require.Len(t, tmpl.SampleValues, 2)
	first := tmpl.SampleValues[0].(map[string]any)
	assert.Equal(t, "body", first["component"])
	assert.Equal(t, "Asha", first["value"])
	assert.Empty(t, e.meta.calls(), "creating a draft doesn't touch Meta")
}

func TestMCP_CreateTemplateWithImageHeaderFromURL(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 64))
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".png") {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer media.Close()
	e, cs := connected(t)
	// Template media goes through the account's Meta app
	require.NoError(t, e.app.DB.Model(&models.WhatsAppAccount{}).Where("organization_id = ?", e.org.ID).Update("app_id", "app-123").Error)
	args := map[string]any{"whatsapp_account": testAccountName(t, e), "name": "court_offer", "language": "en", "category": "MARKETING",
		"header_type": "image", "header_media_url": media.URL + "/page.html", "body": "Book a court this weekend and get 20% off!"}

	res, text := callTool(t, cs, "whatomate_create_template", args)
	assert.True(t, res.IsError)
	assert.Contains(t, text, "image/jpeg or image/png")

	args["header_media_url"] = media.URL + "/court.png"
	res, text = callTool(t, cs, "whatomate_create_template", args)
	require.False(t, res.IsError, text)

	var tmpl models.Template
	require.NoError(t, e.app.DB.Where("organization_id = ? AND name = ?", e.org.ID, "court_offer").First(&tmpl).Error)
	assert.Equal(t, "IMAGE", tmpl.HeaderType)
	assert.Equal(t, "4::aGFuZGxlLWZvci1zYW1wbGUtbWVkaWE=", tmpl.HeaderContent, "the sample's Meta handle is stored")
	assert.Equal(t, 1, countCalls(e.meta.calls(), "/upload:abc"))
}

func TestMCP_SubmitTemplatePreviewsThenSubmits(t *testing.T) {
	e, cs := connected(t)
	res, text := callTool(t, cs, "whatomate_create_template", map[string]any{"whatsapp_account": testAccountName(t, e), "name": "welcome_note",
		"language": "en", "category": "UTILITY", "body": "Welcome to Sportomic, we're glad you're here.", "footer": "Reply STOP to opt out"})
	require.False(t, res.IsError, text)
	id := structured(t, res)["id"].(string)

	res, text = callTool(t, cs, "whatomate_submit_template", map[string]any{"id": id})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "PREVIEW")
	assert.Contains(t, text, "Welcome to Sportomic")
	assert.Contains(t, text, "Reply STOP to opt out")
	assert.Zero(t, countCalls(e.meta.calls(), "/message_templates"), "a preview doesn't submit")

	res, text = callTool(t, cs, "whatomate_submit_template", map[string]any{"id": id, "confirm": true})
	require.False(t, res.IsError, text)
	var tmpl models.Template
	require.NoError(t, e.app.DB.First(&tmpl, "id = ?", id).Error)
	assert.Equal(t, "PENDING", tmpl.Status)
	assert.Equal(t, "meta-tmpl-1", tmpl.MetaTemplateID)
}

func TestMCP_ApprovedTemplateCannotBeEdited(t *testing.T) {
	e, cs := connected(t)
	approved := testTemplateWithStatus(t, e, "APPROVED")

	res, text := callTool(t, cs, "whatomate_update_template", map[string]any{"id": approved.ID.String(), "body": "Changed text here."})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "_v2")
	var after models.Template
	require.NoError(t, e.app.DB.First(&after, "id = ?", approved.ID).Error)
	assert.Equal(t, "APPROVED", after.Status, "an approved template stays approved")

	res, _ = callTool(t, cs, "whatomate_submit_template", map[string]any{"id": approved.ID.String(), "confirm": true})
	assert.True(t, res.IsError)
}

func TestMCP_UpdateDraftTemplateKeepsUntouchedFields(t *testing.T) {
	e, cs := connected(t)
	res, text := callTool(t, cs, "whatomate_create_template", map[string]any{"whatsapp_account": testAccountName(t, e), "name": "slot_reminder",
		"language": "en", "category": "UTILITY", "header_type": "text", "header_text": "Hi {{1}}", "header_example": "Asha",
		"body": "Your slot is at {{1}} today.", "body_examples": map[string]any{"1": "6pm"}, "footer": "Sportomic",
		"buttons": []map[string]any{{"type": "quick_reply", "text": "Cancel"}}})
	require.False(t, res.IsError, text)
	id := structured(t, res)["id"].(string)

	res, text = callTool(t, cs, "whatomate_update_template", map[string]any{"id": id, "body": "Reminder: your slot is at {{1}} today."})
	require.False(t, res.IsError, text)

	var tmpl models.Template
	require.NoError(t, e.app.DB.First(&tmpl, "id = ?", id).Error)
	assert.Equal(t, "Reminder: your slot is at {{1}} today.", tmpl.BodyContent)
	assert.Equal(t, "Hi {{1}}", tmpl.HeaderContent, "the header is kept")
	assert.Equal(t, "Sportomic", tmpl.FooterContent, "the footer is kept")
	assert.Len(t, tmpl.Buttons, 1, "the buttons are kept")
	assert.Len(t, tmpl.SampleValues, 2, "header and body examples are kept")
}

func testTemplateWithStatus(t *testing.T, e *env, status string) models.Template {
	t.Helper()
	tmpl := models.Template{
		OrganizationID: e.org.ID, WhatsAppAccount: testAccountName(t, e), Name: "live_" + strings.ToLower(status),
		Language: "en", Category: "UTILITY", Status: status, BodyContent: "Your booking is confirmed.", MetaTemplateID: "meta-live",
	}
	require.NoError(t, e.app.DB.Create(&tmpl).Error)
	return tmpl
}

func bookingScreens() []map[string]any {
	return []map[string]any{{
		"id": "BOOKING", "title": "Book a court", "terminal": true,
		"layout": map[string]any{"type": "SingleColumnLayout", "children": []any{
			map[string]any{"type": "TextInput", "name": "player_name", "label": "Your name", "required": true},
			map[string]any{"type": "Footer", "label": "Book", "on-click-action": map[string]any{"name": "complete", "payload": map[string]any{"name": "${form.player_name}"}}},
		}},
	}}
}

func TestMCP_WhatsAppFlowLifecycle(t *testing.T) {
	e, cs := connected(t)
	account := testAccountName(t, e)

	res, text := callTool(t, cs, "whatomate_create_whatsapp_flow", map[string]any{"whatsapp_account": account, "name": "Booking", "category": "APPOINTMENT_BOOKING",
		"screens": []map[string]any{{"id": "A", "layout": map[string]any{"type": "SingleColumnLayout", "children": []any{map[string]any{"type": "TextBody", "text": "hi"}}}}}})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "terminal")

	res, text = callTool(t, cs, "whatomate_create_whatsapp_flow", map[string]any{"whatsapp_account": account, "name": "Booking", "category": "APPOINTMENT_BOOKING", "screens": bookingScreens()})
	require.False(t, res.IsError, text)
	id := structured(t, res)["id"].(string)

	// Meta's validation errors come back word for word
	e.meta.rejectFlow = true
	res, text = callTool(t, cs, "whatomate_save_whatsapp_flow_to_meta", map[string]any{"id": id})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "Invalid value for property 'type'")

	e.meta.rejectFlow = false
	res, text = callTool(t, cs, "whatomate_save_whatsapp_flow_to_meta", map[string]any{"id": id})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "meta-flow-1")

	res, text = callTool(t, cs, "whatomate_publish_whatsapp_flow", map[string]any{"id": id})
	require.False(t, res.IsError, text)
	assert.Contains(t, text, "PREVIEW")
	assert.Contains(t, text, "permanent")
	assert.Zero(t, countCalls(e.meta.calls(), "/publish"), "a preview doesn't publish")

	res, text = callTool(t, cs, "whatomate_publish_whatsapp_flow", map[string]any{"id": id, "confirm": true})
	require.False(t, res.IsError, text)
	var flow models.WhatsAppFlow
	require.NoError(t, e.app.DB.First(&flow, "id = ?", id).Error)
	assert.Equal(t, "PUBLISHED", flow.Status)

	res, text = callTool(t, cs, "whatomate_update_whatsapp_flow", map[string]any{"id": id, "name": "Booking v2"})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "duplicate")

	res, text = callTool(t, cs, "whatomate_duplicate_whatsapp_flow", map[string]any{"id": id})
	require.False(t, res.IsError, text)
	copyID := structured(t, res)["id"].(string)
	res, text = callTool(t, cs, "whatomate_update_whatsapp_flow", map[string]any{"id": copyID, "name": "Booking v2"})
	require.False(t, res.IsError, text)
}

func TestMCP_TemplateAndFlowToolsNeedRolePermission(t *testing.T) {
	e, cs := connected(t, "templates:read", "flows.whatsapp:read")

	res, text := callTool(t, cs, "whatomate_create_template", map[string]any{"whatsapp_account": testAccountName(t, e), "name": "x_y",
		"language": "en", "category": "UTILITY", "body": "Hello there friend."})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "templates:write")

	res, text = callTool(t, cs, "whatomate_list_whatsapp_flows", nil)
	assert.False(t, res.IsError, text)

	res, text = callTool(t, cs, "whatomate_create_whatsapp_flow", map[string]any{"whatsapp_account": testAccountName(t, e), "name": "x",
		"category": "OTHER", "screens": bookingScreens()})
	assert.True(t, res.IsError)
	assert.Contains(t, text, "flows.whatsapp:write")
}
