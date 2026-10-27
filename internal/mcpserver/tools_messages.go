package mcpserver

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
)

func init() {
	defineTool(ScopeMessagesRead, "templates:read", &mcp.Tool{
		Name:        "whatomate_list_templates",
		Description: "List WhatsApp message templates. Only APPROVED templates can be sent. Use whatomate_get_template to see a template's text and variables.",
		Annotations: readOnly("List templates"),
	}, listTemplates)

	defineTool(ScopeMessagesRead, "templates:read", &mcp.Tool{
		Name:        "whatomate_get_template",
		Description: "Get a template's header, body, footer, buttons and the variables it needs, so you can fill template_params when sending it.",
		Annotations: readOnly("Get template"),
	}, getTemplate)

	defineTool(ScopeMessagesSend, "chat:write", &mcp.Tool{
		Name: "whatomate_send_text_message",
		Description: "Send a free-form WhatsApp text message to a contact. Only works while the contact's 24-hour service window is open (they messaged within the last 24 hours); " +
			"otherwise use whatomate_send_template_message. Sending is asynchronous: the result has the message ID, and delivery status shows up later in whatomate_get_conversation or whatomate_get_message_log.",
		Annotations: outbound("Send text message", false),
	}, sendTextMessage)

	defineTool(ScopeMessagesSend, "chat:write", &mcp.Tool{
		Name: "whatomate_send_template_message",
		Description: "Send an approved WhatsApp template to a phone number or contact. Works any time, including outside the 24-hour service window. " +
			"Creates the contact if the phone number is new. Templates with image, video or document headers are not supported here; use a campaign or the Whatomate app.",
		Annotations: outbound("Send template message", false),
	}, sendTemplateMessage)

	defineTool(ScopeMessagesRead, "message_logs:read", &mcp.Tool{
		Name: "whatomate_search_message_logs",
		Description: "Search the delivery log of WhatsApp messages (sent and received), newest first, for the last 90 days. Filter by status (e.g. failed), phone, template, source or account " +
			"to answer questions like \"which messages failed yesterday and why\". Pages of 50; pass next_cursor as cursor for more.",
		Annotations: readOnly("Search message logs"),
	}, searchMessageLogs)

	defineTool(ScopeMessagesRead, "message_logs:read", &mcp.Tool{
		Name:        "whatomate_get_message_log",
		Description: "Get one message's full delivery record: content, status with sent/delivered/read times, error, pricing category, campaign and who sent it.",
		Annotations: readOnly("Get message log"),
	}, getMessageLog)

	defineTool(ScopeMessagesRead, "analytics:read", &mcp.Tool{
		Name:        "whatomate_get_message_stats",
		Description: "Organization totals for a date range (default: this month): messages, new contacts, chatbot sessions and campaigns sent, each with the change versus the previous period of the same length.",
		Annotations: readOnly("Get message stats"),
	}, getMessageStats)
}

type listTemplatesInput struct {
	Search          string `json:"search,omitempty" jsonschema:"Part of the template name"`
	Status          string `json:"status,omitempty" jsonschema:"APPROVED, PENDING, REJECTED, ..."`
	Category        string `json:"category,omitempty" jsonschema:"MARKETING, UTILITY or AUTHENTICATION"`
	WhatsAppAccount string `json:"whatsapp_account,omitempty" jsonschema:"Only templates of this WhatsApp account name"`
	Page            int    `json:"page,omitempty" jsonschema:"Page number, starting at 1"`
	Limit           int    `json:"limit,omitempty" jsonschema:"Templates per page, 1-100 (default 30)"`
}

type templateSummary struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Language        string   `json:"language"`
	Category        string   `json:"category"`
	Status          string   `json:"status"`
	WhatsAppAccount string   `json:"whatsapp_account"`
	HeaderType      string   `json:"header_type,omitempty"`
	Variables       []string `json:"variables"`
}

var templateVarRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// templateVars lists the distinct variable names in s, in order.
func templateVars(s string) []string {
	vars := []string{}
	seen := map[string]bool{}
	for _, m := range templateVarRe.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			vars = append(vars, m[1])
		}
	}
	return vars
}

func summarizeTemplate(t handlers.TemplateResponse) templateSummary {
	return templateSummary{
		ID:              t.ID.String(),
		Name:            t.Name,
		Language:        t.Language,
		Category:        t.Category,
		Status:          t.Status,
		WhatsAppAccount: t.WhatsAppAccount,
		HeaderType:      t.HeaderType,
		Variables:       templateVars(t.BodyContent),
	}
}

func listTemplates(c *call, in listTemplatesInput) (string, any, error) {
	q := pageArgs(in.Page, in.Limit, 30, 100)
	setIf(q, "search", in.Search)
	setIf(q, "status", strings.ToUpper(in.Status))
	setIf(q, "category", strings.ToUpper(in.Category))
	setIf(q, "account", in.WhatsAppAccount)

	var resp struct {
		Templates []handlers.TemplateResponse `json:"templates"`
		Total     int64                       `json:"total"`
		Page      int                         `json:"page"`
		Limit     int                         `json:"limit"`
	}
	if err := c.get("/api/templates", q, &resp); err != nil {
		return "", nil, err
	}
	out := struct {
		Templates []templateSummary `json:"templates"`
		Total     int64             `json:"total"`
		Page      int               `json:"page"`
		HasMore   bool              `json:"has_more"`
	}{Templates: make([]templateSummary, len(resp.Templates)), Total: resp.Total, Page: resp.Page}
	out.HasMore = int64(resp.Page*resp.Limit) < resp.Total
	if len(resp.Templates) == 0 {
		return "No templates match.", out, nil
	}
	rows := make([][]string, len(resp.Templates))
	for i, t := range resp.Templates {
		s := summarizeTemplate(t)
		out.Templates[i] = s
		rows[i] = []string{t.Name, t.Language, t.Category, t.Status, t.WhatsAppAccount, strings.Join(s.Variables, ", "), s.ID}
	}
	text := fmt.Sprintf("%d of %d templates (page %d):\n\n", len(resp.Templates), resp.Total, resp.Page) +
		table([]string{"Name", "Language", "Category", "Status", "Account", "Variables", "ID"}, rows)
	if out.HasMore {
		text += fmt.Sprintf("\nMore results: call again with page=%d.", resp.Page+1)
	}
	return text, out, nil
}

type templateIDInput struct {
	TemplateID string `json:"template_id" jsonschema:"Template ID (UUID) from whatomate_list_templates"`
}

func getTemplate(c *call, in templateIDInput) (string, any, error) {
	var t handlers.TemplateResponse
	if err := c.get("/api/templates/"+pathID(in.TemplateID), nil, &t); err != nil {
		return "", nil, err
	}
	s := summarizeTemplate(t)
	out := struct {
		templateSummary
		Header       string `json:"header,omitempty"`
		Body         string `json:"body"`
		Footer       string `json:"footer,omitempty"`
		Buttons      []any  `json:"buttons,omitempty"`
		SampleValues []any  `json:"sample_values,omitempty"`
	}{s, t.HeaderContent, t.BodyContent, t.FooterContent, t.Buttons, t.SampleValues}

	var b strings.Builder
	fmt.Fprintf(&b, "**%s** (%s, %s, %s) on account %s\n\n", t.Name, t.Language, t.Category, t.Status, cell(t.WhatsAppAccount))
	if t.HeaderType != "" {
		fmt.Fprintf(&b, "Header (%s): %s\n\n", t.HeaderType, cell(t.HeaderContent))
	}
	fmt.Fprintf(&b, "Body:\n%s\n\n", t.BodyContent)
	if t.FooterContent != "" {
		fmt.Fprintf(&b, "Footer: %s\n\n", t.FooterContent)
	}
	if len(t.Buttons) > 0 {
		fmt.Fprintf(&b, "Buttons: %d (see structured data)\n\n", len(t.Buttons))
	}
	if len(s.Variables) == 0 {
		b.WriteString("This template has no body variables; send it without template_params.")
	} else {
		fmt.Fprintf(&b, "Body variables: %s. Pass them in template_params keyed by these names, e.g. {%q: \"...\"}.",
			strings.Join(s.Variables, ", "), s.Variables[0])
	}
	if hv := templateVars(t.HeaderContent); t.HeaderType == "TEXT" && len(hv) > 0 {
		fmt.Fprintf(&b, "\nHeader variable: %s. Pass it in header_params.", hv[0])
	}
	if t.HeaderType == "IMAGE" || t.HeaderType == "VIDEO" || t.HeaderType == "DOCUMENT" {
		b.WriteString("\nThis template has a media header, which whatomate_send_template_message cannot fill; use a campaign or the Whatomate app.")
	}
	return b.String(), out, nil
}

type sendTextInput struct {
	ContactID        string `json:"contact_id" jsonschema:"Contact ID (UUID) from whatomate_search_contacts"`
	Text             string `json:"text" jsonschema:"Message text (max 4096 characters)"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty" jsonschema:"Quote this message ID from the conversation"`
	WhatsAppAccount  string `json:"whatsapp_account,omitempty" jsonschema:"Send from this WhatsApp account name instead of the contact's usual one"`
}

type sentMessage struct {
	MessageID string `json:"message_id"`
	ContactID string `json:"contact_id"`
	Status    string `json:"status"`
	Account   string `json:"whatsapp_account,omitempty"`
}

func sendTextMessage(c *call, in sendTextInput) (string, any, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return "", nil, fmt.Errorf("text is empty")
	}
	if n := len([]rune(text)); n > 4096 {
		return "", nil, fmt.Errorf("text is %d characters; WhatsApp allows at most 4096", n)
	}

	var ct handlers.ContactResponse
	if err := c.get("/api/contacts/"+pathID(in.ContactID), nil, &ct); err != nil {
		return "", nil, err
	}
	if !ct.ServiceWindowOpen {
		return "", nil, fmt.Errorf("the 24-hour service window for %s (%s) is closed, so WhatsApp only allows template messages. "+
			"Pick an approved template with whatomate_list_templates and send it with whatomate_send_template_message", contactName(ct), ct.PhoneNumber)
	}

	var body handlers.SendMessageRequest
	body.Type = models.MessageTypeText
	body.Content.Body = text
	body.ReplyToMessageID = in.ReplyToMessageID
	body.WhatsAppAccount = in.WhatsAppAccount

	var msg handlers.MessageResponse
	if err := c.post("/api/contacts/"+pathID(in.ContactID)+"/messages", body, &msg); err != nil {
		return "", nil, err
	}
	c.audit(auditMessages, msg.ID.String(), models.AuditActionCreated, "send_text_message", map[string]any{"to": ct.PhoneNumber})
	out := sentMessage{MessageID: msg.ID.String(), ContactID: msg.ContactID.String(), Status: string(msg.Status), Account: msg.WhatsAppAccount}
	return fmt.Sprintf("Message to %s (%s) accepted with status %q, message ID %s. Delivery status updates arrive asynchronously.",
		contactName(ct), ct.PhoneNumber, out.Status, out.MessageID), out, nil
}

type sendTemplateInput struct {
	PhoneNumber     string            `json:"phone_number,omitempty" jsonschema:"Recipient phone in international format without + or spaces, e.g. 919876543210. Give this or contact_id"`
	ContactID       string            `json:"contact_id,omitempty" jsonschema:"Recipient contact ID (UUID). Give this or phone_number"`
	TemplateName    string            `json:"template_name,omitempty" jsonschema:"Template name. Give this or template_id"`
	TemplateID      string            `json:"template_id,omitempty" jsonschema:"Template ID (UUID). Give this or template_name"`
	TemplateParams  map[string]string `json:"template_params,omitempty" jsonschema:"Body variable values keyed by variable name, e.g. {\"1\": \"Asha\"} or {\"name\": \"Asha\"}"`
	HeaderParams    map[string]string `json:"header_params,omitempty" jsonschema:"Value for a text header variable, keyed by its name"`
	ButtonParams    map[string]string `json:"button_params,omitempty" jsonschema:"Dynamic URL button suffixes keyed by button index, e.g. {\"0\": \"order/123\"}"`
	WhatsAppAccount string            `json:"whatsapp_account,omitempty" jsonschema:"Send from this WhatsApp account name (default: the organization's default)"`
}

func sendTemplateMessage(c *call, in sendTemplateInput) (string, any, error) {
	if in.PhoneNumber == "" && in.ContactID == "" {
		return "", nil, fmt.Errorf("give phone_number or contact_id")
	}
	if in.TemplateName == "" && in.TemplateID == "" {
		return "", nil, fmt.Errorf("give template_name or template_id; whatomate_list_templates lists approved templates")
	}
	body := handlers.SendTemplateMessageRequest{
		ContactID:      in.ContactID,
		PhoneNumber:    strings.TrimPrefix(strings.TrimSpace(in.PhoneNumber), "+"),
		TemplateName:   in.TemplateName,
		TemplateID:     in.TemplateID,
		TemplateParams: in.TemplateParams,
		HeaderParams:   in.HeaderParams,
		ButtonParams:   in.ButtonParams,
		AccountName:    in.WhatsAppAccount,
	}
	var msg handlers.MessageResponse
	if err := c.post("/api/messages/template", body, &msg); err != nil {
		return "", nil, err
	}
	template := in.TemplateName
	if template == "" {
		template = in.TemplateID
	}
	c.audit(auditMessages, msg.ID.String(), models.AuditActionCreated, "send_template_message",
		map[string]any{"to": firstNonEmpty(body.PhoneNumber, in.ContactID), "template": template})
	out := sentMessage{MessageID: msg.ID.String(), ContactID: msg.ContactID.String(), Status: string(msg.Status), Account: msg.WhatsAppAccount}
	return fmt.Sprintf("Template %q accepted with status %q, message ID %s (contact %s). Delivery status updates arrive asynchronously.",
		template, out.Status, out.MessageID, out.ContactID), out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type searchMessageLogsInput struct {
	From            string `json:"from,omitempty" jsonschema:"Start date YYYY-MM-DD or RFC3339 (default: 7 days ago). At most 90 days back"`
	To              string `json:"to,omitempty" jsonschema:"End date YYYY-MM-DD or RFC3339 (default: now)"`
	Status          string `json:"status,omitempty" jsonschema:"pending, sent, delivered, read, failed or received"`
	Direction       string `json:"direction,omitempty" jsonschema:"incoming or outgoing"`
	Source          string `json:"source,omitempty" jsonschema:"api, mcp, agent, chatbot, campaign, automation, phone_app, incoming or call"`
	Phone           string `json:"phone,omitempty" jsonschema:"Contact phone number"`
	Template        string `json:"template,omitempty" jsonschema:"Template name"`
	WhatsAppAccount string `json:"whatsapp_account,omitempty" jsonschema:"WhatsApp account name"`
	Cursor          string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
}

type messageLogSummary struct {
	ID           string     `json:"id"`
	At           time.Time  `json:"at"`
	Direction    string     `json:"direction"`
	Source       string     `json:"source"`
	Status       string     `json:"status"`
	ContactID    string     `json:"contact_id"`
	ContactName  string     `json:"contact_name"`
	ContactPhone string     `json:"contact_phone"`
	Account      string     `json:"whatsapp_account"`
	Type         string     `json:"message_type"`
	Template     string     `json:"template_name,omitempty"`
	Content      string     `json:"content"`
	Error        string     `json:"error,omitempty"`
	Campaign     string     `json:"campaign_name,omitempty"`
	SentAt       *time.Time `json:"sent_at,omitempty"`
	DeliveredAt  *time.Time `json:"delivered_at,omitempty"`
	ReadAt       *time.Time `json:"read_at,omitempty"`
}

func summarizeMessageLog(l handlers.MessageLogResponse, contentLen int) messageLogSummary {
	return messageLogSummary{
		ID:           l.ID.String(),
		At:           l.CreatedAt,
		Direction:    string(l.Direction),
		Source:       string(l.Source),
		Status:       string(l.Status),
		ContactID:    l.ContactID.String(),
		ContactName:  l.ContactName,
		ContactPhone: l.ContactPhone,
		Account:      l.WhatsAppAccount,
		Type:         string(l.MessageType),
		Template:     l.TemplateName,
		Content:      truncate(l.Content, contentLen),
		Error:        l.ErrorMessage,
		Campaign:     l.CampaignName,
		SentAt:       l.SentAt,
		DeliveredAt:  l.DeliveredAt,
		ReadAt:       l.ReadAt,
	}
}

// logRange applies the default range (last 7 days) for log searches.
func logRange(from, to string) (string, string) {
	if from == "" {
		from = time.Now().UTC().AddDate(0, 0, -7).Format(time.RFC3339)
	}
	if to == "" {
		to = time.Now().UTC().Format(time.RFC3339)
	}
	return from, to
}

func searchMessageLogs(c *call, in searchMessageLogsInput) (string, any, error) {
	from, to := logRange(in.From, in.To)
	q := url.Values{"from": {from}, "to": {to}}
	setIf(q, "status", strings.ToLower(in.Status))
	setIf(q, "direction", strings.ToLower(in.Direction))
	setIf(q, "source", strings.ToLower(in.Source))
	setIf(q, "phone", in.Phone)
	setIf(q, "template", in.Template)
	setIf(q, "account", in.WhatsAppAccount)
	setIf(q, "cursor", in.Cursor)

	var resp struct {
		Logs       []handlers.MessageLogResponse `json:"logs"`
		NextCursor string                        `json:"next_cursor"`
	}
	if err := c.get("/api/message-logs", q, &resp); err != nil {
		return "", nil, err
	}
	out := struct {
		Logs       []messageLogSummary `json:"logs"`
		NextCursor string              `json:"next_cursor,omitempty"`
	}{Logs: make([]messageLogSummary, len(resp.Logs)), NextCursor: resp.NextCursor}
	if len(resp.Logs) == 0 {
		return fmt.Sprintf("No messages match between %s and %s.", from, to), out, nil
	}
	rows := make([][]string, len(resp.Logs))
	for i, l := range resp.Logs {
		s := summarizeMessageLog(l, 200)
		out.Logs[i] = s
		what := s.Type
		if s.Template != "" {
			what = "template " + s.Template
		}
		rows[i] = []string{fmtTime(&l.CreatedAt), l.ContactPhone, l.ContactName, s.Direction, s.Source, s.Status,
			what, truncate(l.Content, 60), truncate(l.ErrorMessage, 80), s.ID}
	}
	text := fmt.Sprintf("%d messages between %s and %s, newest first:\n\n", len(resp.Logs), from, to) +
		table([]string{"Time", "Phone", "Name", "Direction", "Source", "Status", "Type", "Content", "Error", "ID"}, rows)
	if resp.NextCursor != "" {
		text += "\nMore results: call again with cursor=" + resp.NextCursor + " and the same filters."
	}
	return text, out, nil
}

type messageLogIDInput struct {
	MessageID string `json:"message_id" jsonschema:"Message ID (UUID) from whatomate_search_message_logs or a send tool"`
}

func getMessageLog(c *call, in messageLogIDInput) (string, any, error) {
	var l handlers.MessageLogResponse
	if err := c.get("/api/message-logs/"+pathID(in.MessageID), nil, &l); err != nil {
		return "", nil, err
	}
	out := struct {
		messageLogSummary
		PricingCategory   string `json:"pricing_category,omitempty"`
		WhatsAppMessageID string `json:"whatsapp_message_id,omitempty"`
		SentBy            string `json:"sent_by,omitempty"`
	}{summarizeMessageLog(l, 4000), l.PricingCategory, l.WhatsAppMessageID, l.SentByUserName}

	var b strings.Builder
	fmt.Fprintf(&b, "%s message, contact %s (%s), account %s\n\n", out.Direction, cell(l.ContactName), l.ContactPhone, cell(l.WhatsAppAccount))
	fmt.Fprintf(&b, "- Status: %s\n- Source: %s\n", out.Status, cell(out.Source))
	fmt.Fprintf(&b, "- Created: %s\n- Sent: %s\n- Delivered: %s\n- Read: %s\n", fmtTime(&l.CreatedAt), fmtTime(l.SentAt), fmtTime(l.DeliveredAt), fmtTime(l.ReadAt))
	if l.TemplateName != "" {
		fmt.Fprintf(&b, "- Template: %s\n", l.TemplateName)
	}
	if l.CampaignName != "" {
		fmt.Fprintf(&b, "- Campaign: %s\n", l.CampaignName)
	}
	if l.SentByUserName != "" {
		fmt.Fprintf(&b, "- Sent by: %s\n", l.SentByUserName)
	}
	if l.PricingCategory != "" {
		fmt.Fprintf(&b, "- Pricing category: %s\n", l.PricingCategory)
	}
	if l.ErrorMessage != "" {
		fmt.Fprintf(&b, "- Error: %s\n", l.ErrorMessage)
	}
	fmt.Fprintf(&b, "\nContent:\n%s", truncate(l.Content, 4000))
	return b.String(), out, nil
}

type statsInput struct {
	From string `json:"from,omitempty" jsonschema:"Start date YYYY-MM-DD (default: first day of this month)"`
	To   string `json:"to,omitempty" jsonschema:"End date YYYY-MM-DD, inclusive (default: today)"`
}

func getMessageStats(c *call, in statsInput) (string, any, error) {
	q := url.Values{}
	if in.From != "" || in.To != "" {
		if in.From == "" || in.To == "" {
			return "", nil, fmt.Errorf("give both from and to, or neither for this month")
		}
		q.Set("from", in.From)
		q.Set("to", in.To)
	}
	var resp struct {
		Stats handlers.DashboardStats `json:"stats"`
	}
	if err := c.get("/api/analytics/dashboard", q, &resp); err != nil {
		return "", nil, err
	}
	s := resp.Stats
	period := "this month"
	if in.From != "" {
		period = in.From + " to " + in.To
	}
	rows := [][]string{
		{"Messages", fmt.Sprint(s.TotalMessages), fmt.Sprintf("%+.1f%%", s.MessagesChange)},
		{"New contacts", fmt.Sprint(s.TotalContacts), fmt.Sprintf("%+.1f%%", s.ContactsChange)},
		{"Chatbot sessions", fmt.Sprint(s.ChatbotSessions), fmt.Sprintf("%+.1f%%", s.ChatbotChange)},
		{"Campaigns sent", fmt.Sprint(s.CampaignsSent), fmt.Sprintf("%+.1f%%", s.CampaignsChange)},
	}
	return "Totals for " + period + ":\n\n" + table([]string{"Metric", "Total", "Change vs previous period"}, rows), s, nil
}
