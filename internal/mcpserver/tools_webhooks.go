package mcpserver

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/handlers"
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
