package mcpserver

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
)

const auditKeywords = models.ResourceChatbotKeywords
const auditAIContexts = models.ResourceChatbotAI
const auditChatbotSettings = models.ResourceSettingsChatbot

func init() {
	defineTool(ScopeChatbotRead, "chatbot.keywords:read", &mcp.Tool{
		Name:        "whatomate_list_keyword_rules",
		Description: "List chatbot keyword rules: when an incoming message matches a rule's keywords, the chatbot replies with the rule's response or hands the chat to an agent.",
		Annotations: readOnly("List keyword rules"),
	}, listKeywordRules)

	defineTool(ScopeChatbotRead, "chatbot.keywords:read", &mcp.Tool{
		Name:        "whatomate_get_keyword_rule",
		Description: "Get one keyword rule in full, including its response text and buttons.",
		Annotations: readOnly("Get keyword rule"),
	}, getKeywordRule)

	defineTool(ScopeChatbotWrite, "chatbot.keywords:write", &mcp.Tool{
		Name: "whatomate_create_keyword_rule",
		Description: "Create a chatbot keyword rule. response_type text replies with response_text (and optional buttons); transfer hands the chat to a human agent, " +
			"sending response_text first. Higher priority rules are checked first.",
		Annotations: write("Create keyword rule", false),
	}, createKeywordRule)

	defineTool(ScopeChatbotWrite, "chatbot.keywords:write", &mcp.Tool{
		Name:        "whatomate_update_keyword_rule",
		Description: "Change a keyword rule. Only the fields you pass change. Disable a rule with enabled=false instead of deleting it.",
		Annotations: write("Update keyword rule", true),
	}, updateKeywordRule)

	defineTool(ScopeChatbotRead, "chatbot.ai:read", &mcp.Tool{
		Name:        "whatomate_list_ai_contexts",
		Description: "List AI contexts: knowledge the chatbot's AI uses when answering, either static text or data fetched from an API, optionally only when trigger keywords match.",
		Annotations: readOnly("List AI contexts"),
	}, listAIContexts)

	defineTool(ScopeChatbotRead, "chatbot.ai:read", &mcp.Tool{
		Name:        "whatomate_get_ai_context",
		Description: "Get one AI context in full.",
		Annotations: readOnly("Get AI context"),
	}, getAIContext)

	defineTool(ScopeChatbotWrite, "chatbot.ai:write", &mcp.Tool{
		Name: "whatomate_create_ai_context",
		Description: "Create an AI context. context_type static gives the AI static_content (FAQ, policies, prices); api fetches data from api_url when used. " +
			"An api context that adds a new URL returns a preview and is saved only when called again with confirm=true after the user approves.",
		Annotations: write("Create AI context", false),
	}, createAIContext)

	defineTool(ScopeChatbotWrite, "chatbot.ai:write", &mcp.Tool{
		Name:        "whatomate_update_ai_context",
		Description: "Change an AI context. Only the fields you pass change. Disable it with enabled=false instead of deleting. A new api_url needs confirm=true after the user approves.",
		Annotations: write("Update AI context", true),
	}, updateAIContext)

	defineTool(ScopeChatbotRead, "settings.chatbot:read", &mcp.Tool{
		Name:        "whatomate_get_chatbot_settings",
		Description: "Get the chatbot's behaviour: on/off, greeting and fallback messages and buttons, session timeout, business hours, out-of-hours message and AI replies.",
		Annotations: readOnly("Get chatbot settings"),
	}, getChatbotSettings)

	defineTool(ScopeChatbotWrite, "settings.chatbot:write", &mcp.Tool{
		Name: "whatomate_update_chatbot_settings",
		Description: "Change the chatbot's behaviour. Only the fields you pass change. The AI provider, model and API key cannot be changed here; " +
			"an admin sets them in Settings → Chatbot.",
		Annotations: write("Update chatbot settings", true),
	}, updateChatbotSettings)
}

// chatButton is a reply or link button on a chatbot message.
type chatButton struct {
	ID          string `json:"id,omitempty" jsonschema:"Reply button ID, used to route the reply (e.g. in a flow edge button:<id>). Required for reply buttons"`
	Title       string `json:"title" jsonschema:"Button label, at most 20 characters"`
	Type        string `json:"type,omitempty" jsonschema:"reply (default), url or phone"`
	URL         string `json:"url,omitempty" jsonschema:"Link for a url button"`
	PhoneNumber string `json:"phone_number,omitempty" jsonschema:"Number for a phone button"`
}

// validateButtons applies WhatsApp's button rules: up to 3 reply buttons, or
// link/phone buttons, never both, with short titles.
func validateButtons(buttons []chatButton) error {
	reply, cta := 0, 0
	for i, b := range buttons {
		if strings.TrimSpace(b.Title) == "" {
			return fmt.Errorf("button %d has no title", i+1)
		}
		if n := len([]rune(b.Title)); n > 20 {
			return fmt.Errorf("button %q is %d characters; WhatsApp allows 20", b.Title, n)
		}
		switch b.Type {
		case "", "reply":
			if b.ID == "" {
				return fmt.Errorf("reply button %q needs an id", b.Title)
			}
			reply++
		case "url":
			if !strings.HasPrefix(b.URL, "https://") && !strings.HasPrefix(b.URL, "http://") {
				return fmt.Errorf("url button %q needs an http(s) url", b.Title)
			}
			cta++
		case "phone":
			if b.PhoneNumber == "" {
				return fmt.Errorf("phone button %q needs a phone_number", b.Title)
			}
			cta++
		default:
			return fmt.Errorf("button %q has unknown type %q; use reply, url or phone", b.Title, b.Type)
		}
	}
	if reply > 3 {
		return fmt.Errorf("%d reply buttons; WhatsApp allows 3", reply)
	}
	if reply > 0 && cta > 0 {
		return fmt.Errorf("WhatsApp cannot mix reply buttons with url or phone buttons in one message")
	}
	return nil
}

func buttonMaps(buttons []chatButton) []map[string]any {
	out := make([]map[string]any, len(buttons))
	for i, b := range buttons {
		m := map[string]any{"title": b.Title}
		if b.Type != "" && b.Type != "reply" {
			m["type"] = b.Type
		}
		if b.ID != "" {
			m["id"] = b.ID
		}
		if b.URL != "" {
			m["url"] = b.URL
		}
		if b.PhoneNumber != "" {
			m["phone_number"] = b.PhoneNumber
		}
		out[i] = m
	}
	return out
}

// Keyword rules

type listChatbotInput struct {
	Search string `json:"search,omitempty" jsonschema:"Part of the name"`
	Page   int    `json:"page,omitempty" jsonschema:"Page number, starting at 1"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Items per page, 1-100 (default 50)"`
}

func listKeywordRules(c *call, in listChatbotInput) (string, any, error) {
	q := pageArgs(in.Page, in.Limit, 50, 100)
	setIf(q, "search", in.Search)
	var resp struct {
		Rules []handlers.KeywordRuleResponse `json:"rules"`
		Total int64                          `json:"total"`
	}
	if err := c.get("/api/chatbot/keywords", q, &resp); err != nil {
		return "", nil, err
	}
	if len(resp.Rules) == 0 {
		return "No keyword rules yet.", resp, nil
	}
	rows := make([][]string, len(resp.Rules))
	for i, r := range resp.Rules {
		rows[i] = []string{r.Name, strings.Join(r.Keywords, ", "), string(r.MatchType), string(r.ResponseType),
			fmt.Sprint(r.Priority), yesNo(r.Enabled), r.ID}
	}
	return fmt.Sprintf("%d of %d keyword rules:\n\n", len(resp.Rules), resp.Total) +
		table([]string{"Name", "Keywords", "Match", "Response", "Priority", "Enabled", "ID"}, rows), resp, nil
}

type idInput struct {
	ID string `json:"id" jsonschema:"ID (UUID) from the matching list tool"`
}

func getKeywordRule(c *call, in idInput) (string, any, error) {
	var r handlers.KeywordRuleResponse
	if err := c.get("/api/chatbot/keywords/"+pathID(in.ID), nil, &r); err != nil {
		return "", nil, err
	}
	var content map[string]any
	_ = json.Unmarshal(r.ResponseContent, &content)
	body, _ := content["body"].(string)
	text := fmt.Sprintf("**%s** (%s, priority %d, enabled: %s)\n\n- Keywords (%s): %s\n- Response: %s\n",
		r.Name, r.ResponseType, r.Priority, yesNo(r.Enabled), r.MatchType, strings.Join(r.Keywords, ", "), cell(body))
	if buttons, ok := content["buttons"].([]any); ok && len(buttons) > 0 {
		b, _ := json.Marshal(buttons)
		text += "- Buttons: " + string(b) + "\n"
	}
	return text, r, nil
}

type keywordRuleInput struct {
	Name         string       `json:"name" jsonschema:"Rule name"`
	Keywords     []string     `json:"keywords" jsonschema:"Words or phrases that trigger the rule"`
	MatchType    string       `json:"match_type,omitempty" jsonschema:"exact, contains (default), starts_with or regex"`
	ResponseType string       `json:"response_type,omitempty" jsonschema:"text (default) or transfer (hand the chat to an agent)"`
	ResponseText string       `json:"response_text" jsonschema:"Message to reply with (for transfer, sent before handing over)"`
	Buttons      []chatButton `json:"buttons,omitempty" jsonschema:"Optional buttons under a text reply"`
	Priority     int          `json:"priority,omitempty" jsonschema:"Higher is checked first (default 0)"`
	Enabled      *bool        `json:"enabled,omitempty" jsonschema:"Default true"`
}

type keywordRuleUpdate struct {
	ID           string       `json:"id" jsonschema:"Keyword rule ID (UUID)"`
	Name         *string      `json:"name,omitempty"`
	Keywords     []string     `json:"keywords,omitempty" jsonschema:"Replaces all keywords"`
	MatchType    *string      `json:"match_type,omitempty" jsonschema:"exact, contains, starts_with or regex"`
	ResponseType *string      `json:"response_type,omitempty" jsonschema:"text or transfer"`
	ResponseText *string      `json:"response_text,omitempty"`
	Buttons      []chatButton `json:"buttons,omitempty" jsonschema:"Replaces the buttons; pass with response_text"`
	Priority     *int         `json:"priority,omitempty"`
	Enabled      *bool        `json:"enabled,omitempty"`
}

var matchTypes = []string{string(models.MatchTypeExact), string(models.MatchTypeContains), string(models.MatchTypeStartsWith), string(models.MatchTypeRegex)}

func checkKeywordFields(matchType, responseType string, keywords []string, buttons []chatButton) error {
	if matchType != "" && !slices.Contains(matchTypes, matchType) {
		return fmt.Errorf("match_type must be one of %s", strings.Join(matchTypes, ", "))
	}
	if responseType != "" && responseType != string(models.ResponseTypeText) && responseType != string(models.ResponseTypeTransfer) {
		return fmt.Errorf("response_type must be text or transfer")
	}
	if matchType == string(models.MatchTypeRegex) {
		for _, k := range keywords {
			if _, err := regexp.Compile(k); err != nil {
				return fmt.Errorf("keyword %q is not a valid regular expression: %v", k, err)
			}
		}
	}
	return validateButtons(buttons)
}

func responseContent(text string, buttons []chatButton) map[string]any {
	content := map[string]any{"body": text}
	if len(buttons) > 0 {
		content["buttons"] = buttonMaps(buttons)
	}
	return content
}

func createKeywordRule(c *call, in keywordRuleInput) (string, any, error) {
	if strings.TrimSpace(in.Name) == "" || len(in.Keywords) == 0 || strings.TrimSpace(in.ResponseText) == "" {
		return "", nil, fmt.Errorf("name, keywords and response_text are required")
	}
	if in.MatchType == "" {
		in.MatchType = string(models.MatchTypeContains)
	}
	if in.ResponseType == "" {
		in.ResponseType = string(models.ResponseTypeText)
	}
	if err := checkKeywordFields(in.MatchType, in.ResponseType, in.Keywords, in.Buttons); err != nil {
		return "", nil, err
	}
	enabled := in.Enabled == nil || *in.Enabled
	body := map[string]any{
		"name": in.Name, "keywords": in.Keywords, "match_type": in.MatchType, "response_type": in.ResponseType,
		"response_content": responseContent(in.ResponseText, in.Buttons), "priority": in.Priority, "enabled": enabled,
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.post("/api/chatbot/keywords", body, &resp); err != nil {
		return "", nil, err
	}
	c.audit(auditKeywords, resp.ID, models.AuditActionCreated, "create_keyword_rule", map[string]any{"name": in.Name})
	return fmt.Sprintf("Created keyword rule %q (ID %s), enabled: %s.", in.Name, resp.ID, yesNo(enabled)), map[string]any{"id": resp.ID}, nil
}

func updateKeywordRule(c *call, in keywordRuleUpdate) (string, any, error) {
	matchType, responseType := "", ""
	if in.MatchType != nil {
		matchType = *in.MatchType
	}
	if in.ResponseType != nil {
		responseType = *in.ResponseType
	}
	if err := checkKeywordFields(matchType, responseType, in.Keywords, in.Buttons); err != nil {
		return "", nil, err
	}
	body := map[string]any{}
	setPtr(body, "name", in.Name)
	setPtr(body, "match_type", in.MatchType)
	setPtr(body, "response_type", in.ResponseType)
	setPtr(body, "priority", in.Priority)
	setPtr(body, "enabled", in.Enabled)
	if in.Keywords != nil {
		body["keywords"] = in.Keywords
	}
	if in.ResponseText != nil {
		body["response_content"] = responseContent(*in.ResponseText, in.Buttons)
	} else if in.Buttons != nil {
		return "", nil, fmt.Errorf("pass response_text together with buttons")
	}
	if len(body) == 0 {
		return "", nil, fmt.Errorf("nothing to change")
	}
	if err := c.put("/api/chatbot/keywords/"+pathID(in.ID), body, nil); err != nil {
		return "", nil, err
	}
	c.audit(auditKeywords, in.ID, models.AuditActionUpdated, "update_keyword_rule", map[string]any{"fields": changedFields(body)})
	return "Updated keyword rule " + in.ID + ": " + changedFields(body) + ".", map[string]any{"id": in.ID}, nil
}

// setPtr sets body[key] when v is a non-nil pointer.
func setPtr[T any](body map[string]any, key string, v *T) {
	if v != nil {
		body[key] = *v
	}
}

func changedFields(body map[string]any) string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return strings.Join(keys, ", ")
}

// AI contexts

func listAIContexts(c *call, in listChatbotInput) (string, any, error) {
	q := pageArgs(in.Page, in.Limit, 50, 100)
	setIf(q, "search", in.Search)
	var resp struct {
		Contexts []handlers.AIContextResponse `json:"contexts"`
		Total    int64                        `json:"total"`
	}
	if err := c.get("/api/chatbot/ai-contexts", q, &resp); err != nil {
		return "", nil, err
	}
	if len(resp.Contexts) == 0 {
		return "No AI contexts yet.", resp, nil
	}
	rows := make([][]string, len(resp.Contexts))
	for i, x := range resp.Contexts {
		rows[i] = []string{x.Name, string(x.ContextType), strings.Join(x.TriggerKeywords, ", "), fmt.Sprint(x.Priority), yesNo(x.Enabled), x.ID}
	}
	return fmt.Sprintf("%d of %d AI contexts:\n\n", len(resp.Contexts), resp.Total) +
		table([]string{"Name", "Type", "Trigger keywords", "Priority", "Enabled", "ID"}, rows), resp, nil
}

func getAIContext(c *call, in idInput) (string, any, error) {
	var x handlers.AIContextResponse
	if err := c.get("/api/chatbot/ai-contexts/"+pathID(in.ID), nil, &x); err != nil {
		return "", nil, err
	}
	text := fmt.Sprintf("**%s** (%s, priority %d, enabled: %s)\n\n- Trigger keywords: %s\n", x.Name, x.ContextType, x.Priority,
		yesNo(x.Enabled), cell(strings.Join(x.TriggerKeywords, ", ")))
	if x.StaticContent != "" {
		text += "\nContent:\n" + truncate(x.StaticContent, 4000) + "\n"
	}
	if u, _ := x.ApiConfig["url"].(string); u != "" {
		text += fmt.Sprintf("- API: %v %s\n", x.ApiConfig["method"], u)
	}
	return text, x, nil
}

type aiContextInput struct {
	Name            string            `json:"name" jsonschema:"Context name"`
	ContextType     string            `json:"context_type,omitempty" jsonschema:"static (default) or api"`
	TriggerKeywords []string          `json:"trigger_keywords,omitempty" jsonschema:"Use this context only when the message contains one of these; empty means always"`
	StaticContent   string            `json:"static_content,omitempty" jsonschema:"For static: the knowledge text"`
	APIURL          string            `json:"api_url,omitempty" jsonschema:"For api: URL to fetch; may use {{variables}}"`
	APIMethod       string            `json:"api_method,omitempty" jsonschema:"For api: GET (default) or POST"`
	APIHeaders      map[string]string `json:"api_headers,omitempty" jsonschema:"For api: request headers"`
	APIBody         string            `json:"api_body,omitempty" jsonschema:"For api: request body template"`
	Priority        int               `json:"priority,omitempty" jsonschema:"Higher is used first (default 0)"`
	Enabled         *bool             `json:"enabled,omitempty" jsonschema:"Default true"`
	Confirm         bool              `json:"confirm,omitempty" jsonschema:"Set true only after the user approved the preview of a new API URL"`
}

type aiContextUpdate struct {
	ID              string            `json:"id" jsonschema:"AI context ID (UUID)"`
	Name            *string           `json:"name,omitempty"`
	ContextType     *string           `json:"context_type,omitempty" jsonschema:"static or api"`
	TriggerKeywords []string          `json:"trigger_keywords,omitempty" jsonschema:"Replaces the trigger keywords"`
	StaticContent   *string           `json:"static_content,omitempty"`
	APIURL          *string           `json:"api_url,omitempty" jsonschema:"Replaces the API settings; pass method, headers and body too if they matter"`
	APIMethod       string            `json:"api_method,omitempty"`
	APIHeaders      map[string]string `json:"api_headers,omitempty"`
	APIBody         string            `json:"api_body,omitempty"`
	Priority        *int              `json:"priority,omitempty"`
	Enabled         *bool             `json:"enabled,omitempty"`
	Confirm         bool              `json:"confirm,omitempty" jsonschema:"Set true only after the user approved the preview of a new API URL"`
}

func apiConfig(url, method string, headers map[string]string, body string) (map[string]any, error) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return nil, fmt.Errorf("api_url must be an http(s) URL")
	}
	method = strings.ToUpper(method)
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "POST" {
		return nil, fmt.Errorf("api_method must be GET or POST")
	}
	cfg := map[string]any{"url": url, "method": method}
	if len(headers) > 0 {
		cfg["headers"] = headers
	}
	if body != "" {
		cfg["body"] = body
	}
	return cfg, nil
}

func createAIContext(c *call, in aiContextInput) (string, any, error) {
	if strings.TrimSpace(in.Name) == "" {
		return "", nil, fmt.Errorf("name is required")
	}
	if in.ContextType == "" {
		in.ContextType = string(models.ContextTypeStatic)
	}
	body := map[string]any{"name": in.Name, "context_type": in.ContextType, "trigger_keywords": nonNil(in.TriggerKeywords),
		"priority": in.Priority, "enabled": in.Enabled == nil || *in.Enabled}
	switch in.ContextType {
	case string(models.ContextTypeStatic):
		if strings.TrimSpace(in.StaticContent) == "" {
			return "", nil, fmt.Errorf("static_content is required for a static context")
		}
		body["static_content"] = in.StaticContent
	case string(models.ContextTypeAPI):
		cfg, err := apiConfig(in.APIURL, in.APIMethod, in.APIHeaders, in.APIBody)
		if err != nil {
			return "", nil, err
		}
		if !in.Confirm {
			text, data := confirmPreview(fmt.Sprintf("Creating AI context %q will fetch data from:", in.Name), []string{cfg["method"].(string) + " " + in.APIURL})
			return text, data, nil
		}
		body["api_config"] = cfg
	default:
		return "", nil, fmt.Errorf("context_type must be static or api")
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.post("/api/chatbot/ai-contexts", body, &resp); err != nil {
		return "", nil, err
	}
	c.audit(auditAIContexts, resp.ID, models.AuditActionCreated, "create_ai_context", map[string]any{"name": in.Name})
	return fmt.Sprintf("Created AI context %q (ID %s).", in.Name, resp.ID), map[string]any{"id": resp.ID}, nil
}

func updateAIContext(c *call, in aiContextUpdate) (string, any, error) {
	body := map[string]any{}
	setPtr(body, "name", in.Name)
	setPtr(body, "context_type", in.ContextType)
	setPtr(body, "static_content", in.StaticContent)
	setPtr(body, "priority", in.Priority)
	setPtr(body, "enabled", in.Enabled)
	if in.ContextType != nil && *in.ContextType != string(models.ContextTypeStatic) && *in.ContextType != string(models.ContextTypeAPI) {
		return "", nil, fmt.Errorf("context_type must be static or api")
	}
	if in.TriggerKeywords != nil {
		body["trigger_keywords"] = in.TriggerKeywords
	}
	if in.APIURL != nil {
		cfg, err := apiConfig(*in.APIURL, in.APIMethod, in.APIHeaders, in.APIBody)
		if err != nil {
			return "", nil, err
		}
		var current handlers.AIContextResponse
		if err := c.get("/api/chatbot/ai-contexts/"+pathID(in.ID), nil, &current); err != nil {
			return "", nil, err
		}
		existing, _ := current.ApiConfig["url"].(string)
		if added := newOutboundURLs([]string{*in.APIURL}, []string{existing}); len(added) > 0 && !in.Confirm {
			text, data := confirmPreview(fmt.Sprintf("Updating AI context %q will fetch data from a new address:", current.Name), added)
			return text, data, nil
		}
		body["api_config"] = cfg
	}
	if len(body) == 0 {
		return "", nil, fmt.Errorf("nothing to change")
	}
	if err := c.put("/api/chatbot/ai-contexts/"+pathID(in.ID), body, nil); err != nil {
		return "", nil, err
	}
	c.audit(auditAIContexts, in.ID, models.AuditActionUpdated, "update_ai_context", map[string]any{"fields": changedFields(body)})
	return "Updated AI context " + in.ID + ": " + changedFields(body) + ".", map[string]any{"id": in.ID}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Chatbot settings

// chatbotSettingsView is the part of the chatbot settings assistants see and
// change; the AI provider, model and key stay with admins.
type chatbotSettingsView struct {
	Enabled                    bool             `json:"enabled"`
	GreetingMessage            string           `json:"greeting_message"`
	GreetingButtons            []map[string]any `json:"greeting_buttons"`
	FallbackMessage            string           `json:"fallback_message"`
	FallbackButtons            []map[string]any `json:"fallback_buttons"`
	SessionTimeoutMinutes      int              `json:"session_timeout_minutes"`
	BusinessHoursEnabled       bool             `json:"business_hours_enabled"`
	BusinessHours              []map[string]any `json:"business_hours"`
	OutOfHoursMessage          string           `json:"out_of_hours_message"`
	AllowAutomatedOutsideHours bool             `json:"allow_automated_outside_hours"`
	AIEnabled                  bool             `json:"ai_enabled"`
	AISystemPrompt             string           `json:"ai_system_prompt"`
}

func getChatbotSettings(c *call, _ noInput) (string, any, error) {
	var resp struct {
		Settings chatbotSettingsView `json:"settings"`
	}
	if err := c.get("/api/chatbot/settings", nil, &resp); err != nil {
		return "", nil, err
	}
	s := resp.Settings
	var b strings.Builder
	fmt.Fprintf(&b, "- Chatbot on: %s\n- Greeting: %s\n- Fallback: %s\n- Session timeout: %d min\n", yesNo(s.Enabled),
		cell(truncate(s.GreetingMessage, 300)), cell(truncate(s.FallbackMessage, 300)), s.SessionTimeoutMinutes)
	fmt.Fprintf(&b, "- Business hours: %s", yesNo(s.BusinessHoursEnabled))
	if s.BusinessHoursEnabled {
		hours, _ := json.Marshal(s.BusinessHours)
		fmt.Fprintf(&b, " %s; out of hours: %s; automation outside hours: %s", hours, cell(truncate(s.OutOfHoursMessage, 200)), yesNo(s.AllowAutomatedOutsideHours))
	}
	fmt.Fprintf(&b, "\n- AI replies: %s\n", yesNo(s.AIEnabled))
	if s.AISystemPrompt != "" {
		fmt.Fprintf(&b, "- AI system prompt: %s\n", truncate(s.AISystemPrompt, 1000))
	}
	return b.String(), s, nil
}

// businessHour is one day of the chatbot's business hours.
type businessHour struct {
	Day       int    `json:"day" jsonschema:"0 = Sunday, 1 = Monday … 6 = Saturday"`
	Enabled   bool   `json:"enabled" jsonschema:"Open this day"`
	StartTime string `json:"start_time" jsonschema:"Opening time HH:MM, 24-hour"`
	EndTime   string `json:"end_time" jsonschema:"Closing time HH:MM, 24-hour"`
}

type chatbotSettingsInput struct {
	Enabled                    *bool          `json:"enabled,omitempty" jsonschema:"Turn the chatbot on or off"`
	GreetingMessage            *string        `json:"greeting_message,omitempty" jsonschema:"Sent when a contact starts a conversation"`
	GreetingButtons            []chatButton   `json:"greeting_buttons,omitempty" jsonschema:"Replaces the greeting buttons"`
	FallbackMessage            *string        `json:"fallback_message,omitempty" jsonschema:"Sent when nothing matches"`
	FallbackButtons            []chatButton   `json:"fallback_buttons,omitempty" jsonschema:"Replaces the fallback buttons"`
	SessionTimeoutMinutes      *int           `json:"session_timeout_minutes,omitempty" jsonschema:"Minutes of silence before a chatbot session ends"`
	BusinessHoursEnabled       *bool          `json:"business_hours_enabled,omitempty"`
	BusinessHours              []businessHour `json:"business_hours,omitempty" jsonschema:"Replaces the whole week; days not listed count as closed"`
	OutOfHoursMessage          *string        `json:"out_of_hours_message,omitempty"`
	AllowAutomatedOutsideHours *bool          `json:"allow_automated_outside_hours,omitempty" jsonschema:"Keep keyword rules and flows running outside business hours"`
	AIEnabled                  *bool          `json:"ai_enabled,omitempty" jsonschema:"Let the AI answer messages nothing else matched"`
	AISystemPrompt             *string        `json:"ai_system_prompt,omitempty" jsonschema:"Instructions for the AI's replies"`
}

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func updateChatbotSettings(c *call, in chatbotSettingsInput) (string, any, error) {
	body := map[string]any{}
	setPtr(body, "enabled", in.Enabled)
	setPtr(body, "greeting_message", in.GreetingMessage)
	setPtr(body, "fallback_message", in.FallbackMessage)
	setPtr(body, "business_hours_enabled", in.BusinessHoursEnabled)
	setPtr(body, "out_of_hours_message", in.OutOfHoursMessage)
	setPtr(body, "allow_automated_outside_hours", in.AllowAutomatedOutsideHours)
	setPtr(body, "ai_enabled", in.AIEnabled)
	setPtr(body, "ai_system_prompt", in.AISystemPrompt)
	if in.SessionTimeoutMinutes != nil {
		if *in.SessionTimeoutMinutes < 1 {
			return "", nil, fmt.Errorf("session_timeout_minutes must be at least 1")
		}
		body["session_timeout_minutes"] = *in.SessionTimeoutMinutes
	}
	for key, buttons := range map[string][]chatButton{"greeting_buttons": in.GreetingButtons, "fallback_buttons": in.FallbackButtons} {
		if buttons == nil {
			continue
		}
		if err := validateButtons(buttons); err != nil {
			return "", nil, fmt.Errorf("%s: %w", key, err)
		}
		body[key] = buttonMaps(buttons)
	}
	if in.BusinessHours != nil {
		hours := make([]map[string]any, len(in.BusinessHours))
		for i, h := range in.BusinessHours {
			if h.Day < 0 || h.Day > 6 {
				return "", nil, fmt.Errorf("business_hours day must be 0 (Sunday) to 6 (Saturday)")
			}
			if h.Enabled && (!hhmm.MatchString(h.StartTime) || !hhmm.MatchString(h.EndTime) || h.EndTime <= h.StartTime) {
				return "", nil, fmt.Errorf("business_hours day %d needs start_time before end_time, both HH:MM", h.Day)
			}
			hours[i] = map[string]any{"day": h.Day, "enabled": h.Enabled, "start_time": h.StartTime, "end_time": h.EndTime}
		}
		body["business_hours"] = hours
	}
	if len(body) == 0 {
		return "", nil, fmt.Errorf("nothing to change")
	}
	if err := c.put("/api/chatbot/settings", body, nil); err != nil {
		return "", nil, err
	}
	c.audit(auditChatbotSettings, c.p.OrgID.String(), models.AuditActionUpdated, "update_chatbot_settings", map[string]any{"fields": changedFields(body)})
	return "Updated chatbot settings: " + changedFields(body) + ".", map[string]any{"updated": changedFields(body)}, nil
}
