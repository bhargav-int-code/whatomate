package mcpserver

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
)

func init() {
	defineTool(ScopeWebhooksRead, "webhook_logs:read", &mcp.Tool{
		Name: "whatomate_search_webhook_logs",
		Description: "Search deliveries of Whatomate's outbound webhooks (events sent to your own systems), newest first, for the last 90 days. " +
			"Filter by status (e.g. failed), event, webhook or a search term matching the reference ID or phone. Pages of 50; pass next_cursor as cursor for more.",
		Annotations: readOnly("Search webhook logs"),
	}, searchWebhookLogs)

	defineTool(ScopeWebhooksRead, "webhook_logs:read", &mcp.Tool{
		Name:        "whatomate_get_webhook_log",
		Description: "Get one webhook delivery in full: the request sent (URL, headers, body), the receiver's response and every attempt.",
		Annotations: readOnly("Get webhook log"),
	}, getWebhookLog)

	defineTool(ScopeWebhooksRead, "webhooks:read", &mcp.Tool{
		Name:        "whatomate_list_webhooks",
		Description: "List the organization's outbound webhooks (where Whatomate sends events) and every event they can subscribe to.",
		Annotations: readOnly("List webhooks"),
	}, listWebhooks)

	defineTool(ScopeWebhooksRead, "webhooks:read", &mcp.Tool{
		Name:        "whatomate_get_webhook",
		Description: "Get one outbound webhook: URL, events, header names and whether it is active. Secrets are never shown.",
		Annotations: readOnly("Get webhook"),
	}, getWebhook)

	defineTool(ScopeWebhooksWrite, "webhooks:write", &mcp.Tool{
		Name: "whatomate_create_webhook",
		Description: "Create an outbound webhook that sends the chosen events (e.g. message.incoming, message.failed) to a URL. " +
			"Always returns a preview first; it is created only when called again with confirm=true after the user approves the URL and events.",
		Annotations: &mcp.ToolAnnotations{Title: "Create webhook", DestructiveHint: falsePtr, OpenWorldHint: truePtr},
	}, createWebhook)

	defineTool(ScopeWebhooksWrite, "webhooks:write", &mcp.Tool{
		Name: "whatomate_update_webhook",
		Description: "Change an outbound webhook. Only the fields you pass change; deactivate one with active=false instead of deleting it. " +
			"Always returns a preview first; it is saved only when called again with confirm=true after the user approves.",
		Annotations: &mcp.ToolAnnotations{Title: "Update webhook", DestructiveHint: falsePtr, IdempotentHint: true, OpenWorldHint: truePtr},
	}, updateWebhook)

	defineTool(ScopeWebhooksWrite, "webhooks:write", &mcp.Tool{
		Name:        "whatomate_test_webhook",
		Description: "Send a test event to a webhook and report whether the receiver accepted it.",
		Annotations: &mcp.ToolAnnotations{Title: "Test webhook", DestructiveHint: falsePtr, IdempotentHint: true, OpenWorldHint: truePtr},
	}, testWebhook)
}

type searchWebhookLogsInput struct {
	From      string `json:"from,omitempty" jsonschema:"Start date YYYY-MM-DD or RFC3339 (default: 7 days ago). At most 90 days back"`
	To        string `json:"to,omitempty" jsonschema:"End date YYYY-MM-DD or RFC3339 (default: now)"`
	Status    string `json:"status,omitempty" jsonschema:"processing, success or failed"`
	Event     string `json:"event,omitempty" jsonschema:"Event name, e.g. message.incoming or message.failed"`
	WebhookID string `json:"webhook_id,omitempty" jsonschema:"Only deliveries of this webhook (UUID)"`
	Search    string `json:"search,omitempty" jsonschema:"Reference ID (message, contact or transfer ID) or phone number"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
}

type webhookLogSummary struct {
	ID             string     `json:"id"`
	At             time.Time  `json:"at"`
	Webhook        string     `json:"webhook_name"`
	WebhookID      string     `json:"webhook_id"`
	Event          string     `json:"event"`
	ReferenceID    string     `json:"reference_id,omitempty"`
	ContactPhone   string     `json:"contact_phone,omitempty"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	ResponseStatus int        `json:"response_status,omitempty"`
	FailureReason  string     `json:"failure_reason,omitempty"`
	DurationMs     int        `json:"duration_ms"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

func summarizeWebhookLog(l handlers.WebhookLogResponse) webhookLogSummary {
	return webhookLogSummary{
		ID:             l.ID.String(),
		At:             l.CreatedAt,
		Webhook:        l.WebhookName,
		WebhookID:      l.WebhookID.String(),
		Event:          l.Event,
		ReferenceID:    l.ReferenceID,
		ContactPhone:   l.ContactPhone,
		Status:         string(l.Status),
		Attempts:       l.AttemptCount,
		ResponseStatus: l.ResponseStatus,
		FailureReason:  l.FailureReason,
		DurationMs:     l.DurationMs,
		CompletedAt:    l.CompletedAt,
	}
}

func searchWebhookLogs(c *call, in searchWebhookLogsInput) (string, any, error) {
	from, to := logRange(in.From, in.To)
	q := url.Values{"from": {from}, "to": {to}}
	setIf(q, "status", strings.ToLower(in.Status))
	setIf(q, "event", in.Event)
	setIf(q, "webhook_id", in.WebhookID)
	setIf(q, "search", in.Search)
	setIf(q, "cursor", in.Cursor)

	var resp struct {
		Logs       []handlers.WebhookLogResponse `json:"logs"`
		NextCursor string                        `json:"next_cursor"`
	}
	if err := c.get("/api/webhook-logs", q, &resp); err != nil {
		return "", nil, err
	}
	out := struct {
		Logs       []webhookLogSummary `json:"logs"`
		NextCursor string              `json:"next_cursor,omitempty"`
	}{Logs: make([]webhookLogSummary, len(resp.Logs)), NextCursor: resp.NextCursor}
	if len(resp.Logs) == 0 {
		return fmt.Sprintf("No webhook deliveries match between %s and %s.", from, to), out, nil
	}
	rows := make([][]string, len(resp.Logs))
	for i, l := range resp.Logs {
		s := summarizeWebhookLog(l)
		out.Logs[i] = s
		code := ""
		if s.ResponseStatus != 0 {
			code = fmt.Sprint(s.ResponseStatus)
		}
		rows[i] = []string{fmtTime(&l.CreatedAt), s.Webhook, s.Event, s.Status, fmt.Sprint(s.Attempts), code,
			truncate(s.FailureReason, 80), s.ContactPhone, s.ID}
	}
	text := fmt.Sprintf("%d webhook deliveries between %s and %s, newest first:\n\n", len(resp.Logs), from, to) +
		table([]string{"Time", "Webhook", "Event", "Status", "Attempts", "HTTP", "Failure", "Phone", "ID"}, rows)
	if resp.NextCursor != "" {
		text += "\nMore results: call again with cursor=" + resp.NextCursor + " and the same filters."
	}
	return text, out, nil
}

type webhookLogIDInput struct {
	DeliveryID string `json:"delivery_id" jsonschema:"Delivery ID (UUID) from whatomate_search_webhook_logs"`
}

func getWebhookLog(c *call, in webhookLogIDInput) (string, any, error) {
	var l handlers.WebhookLogResponse
	if err := c.get("/api/webhook-logs/"+pathID(in.DeliveryID), nil, &l); err != nil {
		return "", nil, err
	}
	out := struct {
		webhookLogSummary
		Request     *handlers.WebhookLogRequest      `json:"request,omitempty"`
		Response    *handlers.WebhookLogResponseBody `json:"response,omitempty"`
		AttemptList any                              `json:"attempt_list,omitempty"`
	}{summarizeWebhookLog(l), l.Request, l.Response, l.Attempts}

	var b strings.Builder
	fmt.Fprintf(&b, "**%s** delivery of %s to webhook %s: %s after %d attempt(s), %d ms\n\n",
		l.Event, cell(l.ReferenceID), cell(l.WebhookName), l.Status, l.AttemptCount, l.DurationMs)
	if l.FailureReason != "" {
		fmt.Fprintf(&b, "Failure: %s\n\n", l.FailureReason)
	}
	if l.Request != nil {
		fmt.Fprintf(&b, "Request: %s %s\nBody: %s\n\n", l.Request.Method, l.Request.URL, truncate(string(l.Request.Body), 2000))
	}
	if l.Response != nil {
		fmt.Fprintf(&b, "Response: HTTP %d\n%s\n", l.Response.StatusCode, truncate(l.Response.Body, 2000))
	}
	return b.String(), out, nil
}

// Webhook configuration

// webhookView is a webhook as assistants see it: header values may be
// credentials, so only their names are shown.
type webhookView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	HeaderNames []string `json:"header_names"`
	Active      bool     `json:"active"`
	HasSecret   bool     `json:"has_secret"`
}

func toWebhookView(w handlers.WebhookResponse) webhookView {
	names := make([]string, 0, len(w.Headers))
	for k := range w.Headers {
		names = append(names, k)
	}
	slices.Sort(names)
	return webhookView{ID: w.ID.String(), Name: w.Name, URL: w.URL, Events: w.Events, HeaderNames: names, Active: w.IsActive, HasSecret: w.HasSecret}
}

func webhookEventNames() []string {
	names := make([]string, len(handlers.AvailableWebhookEvents))
	for i, e := range handlers.AvailableWebhookEvents {
		names[i] = e["value"]
	}
	return names
}

func checkWebhookEvents(events []string) error {
	known := webhookEventNames()
	for _, e := range events {
		if !slices.Contains(known, e) {
			return fmt.Errorf("unknown event %q; events are %s", e, strings.Join(known, ", "))
		}
	}
	return nil
}

func listWebhooks(c *call, _ noInput) (string, any, error) {
	var resp struct {
		Webhooks []handlers.WebhookResponse `json:"webhooks"`
	}
	q := url.Values{"limit": {"100"}}
	if err := c.get("/api/webhooks", q, &resp); err != nil {
		return "", nil, err
	}
	views := make([]webhookView, len(resp.Webhooks))
	rows := make([][]string, len(resp.Webhooks))
	for i, w := range resp.Webhooks {
		views[i] = toWebhookView(w)
		rows[i] = []string{w.Name, w.URL, strings.Join(w.Events, ", "), yesNo(w.IsActive), views[i].ID}
	}
	var b strings.Builder
	if len(views) == 0 {
		b.WriteString("No webhooks yet.\n")
	} else {
		b.WriteString(table([]string{"Name", "URL", "Events", "Active", "ID"}, rows))
	}
	b.WriteString("\nAvailable events:\n")
	for _, e := range handlers.AvailableWebhookEvents {
		fmt.Fprintf(&b, "- %s: %s\n", e["value"], e["description"])
	}
	return b.String(), map[string]any{"webhooks": views, "available_events": webhookEventNames()}, nil
}

func (c *call) webhook(id string) (handlers.WebhookResponse, error) {
	var w handlers.WebhookResponse
	err := c.get("/api/webhooks/"+pathID(id), nil, &w)
	return w, err
}

func getWebhook(c *call, in idInput) (string, any, error) {
	w, err := c.webhook(in.ID)
	if err != nil {
		return "", nil, err
	}
	v := toWebhookView(w)
	return fmt.Sprintf("**%s**\n\n- URL: %s\n- Events: %s\n- Headers: %s\n- Signing secret: %s\n- Active: %s\n",
		v.Name, v.URL, strings.Join(v.Events, ", "), cell(strings.Join(v.HeaderNames, ", ")), yesNo(v.HasSecret), yesNo(v.Active)), v, nil
}

type createWebhookInput struct {
	Name    string            `json:"name" jsonschema:"Webhook name"`
	URL     string            `json:"url" jsonschema:"HTTPS URL that receives the events"`
	Events  []string          `json:"events" jsonschema:"Events to send; whatomate_list_webhooks lists them"`
	Headers map[string]string `json:"headers,omitempty" jsonschema:"Extra request headers, e.g. an auth token for the receiver"`
	Secret  string            `json:"secret,omitempty" jsonschema:"Optional signing secret; requests then carry an X-Webhook-Signature HMAC"`
	Confirm bool              `json:"confirm,omitempty" jsonschema:"Set true only after the user approved the preview"`
}

func createWebhook(c *call, in createWebhookInput) (string, any, error) {
	if strings.TrimSpace(in.Name) == "" || in.URL == "" || len(in.Events) == 0 {
		return "", nil, fmt.Errorf("name, url and events are required")
	}
	if err := checkWebhookEvents(in.Events); err != nil {
		return "", nil, err
	}
	if !in.Confirm {
		details := []string{"URL: " + in.URL, "Events: " + strings.Join(in.Events, ", ")}
		if len(in.Headers) > 0 {
			details = append(details, "Headers: "+strings.Join(mapKeys(in.Headers), ", "))
		}
		text, data := confirmPreview(fmt.Sprintf("Creating webhook %q sends these events, including message contents and phone numbers, to:", in.Name), details)
		return text, data, nil
	}
	body := handlers.WebhookRequest{Name: in.Name, URL: in.URL, Events: in.Events, Headers: in.Headers, Secret: in.Secret, IsActive: true}
	var w handlers.WebhookResponse
	if err := c.post("/api/webhooks", body, &w); err != nil {
		return "", nil, err
	}
	c.audit(auditWebhooks, w.ID.String(), models.AuditActionCreated, "create_webhook", map[string]any{"url": w.URL, "events": strings.Join(w.Events, ", ")})
	return fmt.Sprintf("Created webhook %q (ID %s). Send a test with whatomate_test_webhook.", w.Name, w.ID), toWebhookView(w), nil
}

type updateWebhookInput struct {
	ID      string            `json:"id" jsonschema:"Webhook ID (UUID)"`
	Name    string            `json:"name,omitempty"`
	URL     string            `json:"url,omitempty"`
	Events  []string          `json:"events,omitempty" jsonschema:"Replaces the events"`
	Headers map[string]string `json:"headers,omitempty" jsonschema:"Replaces all extra headers"`
	Secret  string            `json:"secret,omitempty" jsonschema:"New signing secret"`
	Active  *bool             `json:"active,omitempty" jsonschema:"false pauses deliveries"`
	Confirm bool              `json:"confirm,omitempty" jsonschema:"Set true only after the user approved the preview"`
}

func updateWebhook(c *call, in updateWebhookInput) (string, any, error) {
	if err := checkWebhookEvents(in.Events); err != nil {
		return "", nil, err
	}
	current, err := c.webhook(in.ID)
	if err != nil {
		return "", nil, err
	}
	var changes []string
	if in.Name != "" && in.Name != current.Name {
		changes = append(changes, fmt.Sprintf("Name: %s → %s", current.Name, in.Name))
	}
	if in.URL != "" && in.URL != current.URL {
		changes = append(changes, fmt.Sprintf("URL: %s → %s", current.URL, in.URL))
	}
	if in.Events != nil {
		changes = append(changes, fmt.Sprintf("Events: %s → %s", strings.Join(current.Events, ", "), strings.Join(in.Events, ", ")))
	}
	if in.Headers != nil {
		changes = append(changes, "Headers: "+cell(strings.Join(mapKeys(in.Headers), ", ")))
	}
	if in.Secret != "" {
		changes = append(changes, "Signing secret: replaced")
	}
	active := current.IsActive
	if in.Active != nil && *in.Active != current.IsActive {
		active = *in.Active
		changes = append(changes, fmt.Sprintf("Active: %s → %s", yesNo(current.IsActive), yesNo(active)))
	}
	if len(changes) == 0 {
		return "", nil, fmt.Errorf("nothing to change")
	}
	if !in.Confirm {
		text, data := confirmPreview(fmt.Sprintf("Updating webhook %q (%s):", current.Name, current.URL), changes)
		return text, data, nil
	}
	// The REST endpoint always applies is_active, so send the current value
	// unless it is being changed
	body := handlers.WebhookRequest{Name: in.Name, URL: in.URL, Events: in.Events, Headers: in.Headers, Secret: in.Secret, IsActive: active}
	var w handlers.WebhookResponse
	if err := c.put("/api/webhooks/"+pathID(in.ID), body, &w); err != nil {
		return "", nil, err
	}
	c.audit(auditWebhooks, in.ID, models.AuditActionUpdated, "update_webhook", map[string]any{"changes": strings.Join(changes, "; ")})
	return "Updated webhook " + w.Name + ":\n- " + strings.Join(changes, "\n- "), toWebhookView(w), nil
}

func testWebhook(c *call, in idInput) (string, any, error) {
	if err := c.post("/api/webhooks/"+pathID(in.ID)+"/test", nil, nil); err != nil {
		return "", nil, err
	}
	return "The receiver accepted the test event.", map[string]any{"ok": true}, nil
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
