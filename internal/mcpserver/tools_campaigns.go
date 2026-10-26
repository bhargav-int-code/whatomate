package mcpserver

import (
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
)

// maxRecipientsPerCall bounds one whatomate_add_campaign_recipients call.
const maxRecipientsPerCall = 1000

func init() {
	defineTool(ScopeCampaignsRead, "campaigns:read", &mcp.Tool{
		Name:        "whatomate_list_campaigns",
		Description: "List template campaigns with their status and sent/delivered/read/failed counts.",
		Annotations: readOnly("List campaigns"),
	}, listCampaigns)

	defineTool(ScopeCampaignsRead, "campaigns:read", &mcp.Tool{
		Name:        "whatomate_get_campaign",
		Description: "Get one campaign's status, template, account, schedule and delivery counts.",
		Annotations: readOnly("Get campaign"),
	}, getCampaign)

	defineTool(ScopeCampaignsRead, "campaigns:read", &mcp.Tool{
		Name:        "whatomate_list_campaign_recipients",
		Description: "List a campaign's recipients with each one's delivery status and error, optionally only those with a given status (e.g. failed).",
		Annotations: readOnly("List campaign recipients"),
	}, listCampaignRecipients)

	defineTool(ScopeCampaignsWrite, "campaigns:write", &mcp.Tool{
		Name:        "whatomate_create_campaign",
		Description: "Create a draft campaign that will send one approved template. Nothing is sent until recipients are added and whatomate_start_campaign is confirmed.",
		Annotations: write("Create campaign", false),
	}, createCampaign)

	defineTool(ScopeCampaignsWrite, "campaigns:write", &mcp.Tool{
		Name:        "whatomate_add_campaign_recipients",
		Description: fmt.Sprintf("Add recipients to a draft campaign, each with its own template variable values. Up to %d per call; call repeatedly for more.", maxRecipientsPerCall),
		Annotations: write("Add campaign recipients", false),
	}, addCampaignRecipients)

	defineTool(ScopeCampaignsWrite, "campaigns:execute", &mcp.Tool{
		Name: "whatomate_start_campaign",
		Description: "Start (or resume) sending a campaign. Without confirm=true this only returns a preview of what would be sent and to how many people; " +
			"show the preview to the user and call again with confirm=true only after they approve.",
		Annotations: outbound("Start campaign", true),
	}, startCampaign)

	defineTool(ScopeCampaignsWrite, "campaigns:execute", &mcp.Tool{
		Name:        "whatomate_pause_campaign",
		Description: "Pause a running campaign. Messages already sent are not affected; resume with whatomate_start_campaign.",
		Annotations: write("Pause campaign", true),
	}, pauseCampaign)

	defineTool(ScopeCampaignsWrite, "campaigns:execute", &mcp.Tool{
		Name:        "whatomate_cancel_campaign",
		Description: "Cancel a campaign for good. Recipients not yet messaged are never sent to. This cannot be undone.",
		Annotations: &mcp.ToolAnnotations{Title: "Cancel campaign", DestructiveHint: truePtr, IdempotentHint: true, OpenWorldHint: falsePtr},
	}, cancelCampaign)
}

type campaignSummary struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	Template        string `json:"template_name,omitempty"`
	TemplateID      string `json:"template_id"`
	WhatsAppAccount string `json:"whatsapp_account"`
	TotalRecipients int    `json:"total_recipients"`
	Sent            int    `json:"sent"`
	Delivered       int    `json:"delivered"`
	Read            int    `json:"read"`
	Failed          int    `json:"failed"`
	ScheduledAt     string `json:"scheduled_at,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	CompletedAt     string `json:"completed_at,omitempty"`
	CreatedBy       string `json:"created_by,omitempty"`
}

func summarizeCampaign(cp handlers.CampaignResponse) campaignSummary {
	s := campaignSummary{
		ID:              cp.ID.String(),
		Name:            cp.Name,
		Status:          string(cp.Status),
		Template:        cp.TemplateName,
		TemplateID:      cp.TemplateID.String(),
		WhatsAppAccount: cp.WhatsAppAccount,
		TotalRecipients: cp.TotalRecipients,
		Sent:            cp.SentCount,
		Delivered:       cp.DeliveredCount,
		Read:            cp.ReadCount,
		Failed:          cp.FailedCount,
		CreatedBy:       cp.CreatedByName,
	}
	if cp.ScheduledAt != nil {
		s.ScheduledAt = fmtTime(cp.ScheduledAt)
	}
	if cp.StartedAt != nil {
		s.StartedAt = fmtTime(cp.StartedAt)
	}
	if cp.CompletedAt != nil {
		s.CompletedAt = fmtTime(cp.CompletedAt)
	}
	return s
}

type listCampaignsInput struct {
	Search          string `json:"search,omitempty" jsonschema:"Part of the campaign name"`
	Status          string `json:"status,omitempty" jsonschema:"draft, scheduled, queued, processing, paused, completed, cancelled or failed"`
	WhatsAppAccount string `json:"whatsapp_account,omitempty" jsonschema:"WhatsApp account name"`
	Page            int    `json:"page,omitempty" jsonschema:"Page number, starting at 1"`
	Limit           int    `json:"limit,omitempty" jsonschema:"Campaigns per page, 1-100 (default 20)"`
}

func listCampaigns(c *call, in listCampaignsInput) (string, any, error) {
	q := pageArgs(in.Page, in.Limit, 20, 100)
	setIf(q, "search", in.Search)
	setIf(q, "status", strings.ToLower(in.Status))
	setIf(q, "whatsapp_account", in.WhatsAppAccount)

	var resp struct {
		Campaigns []handlers.CampaignResponse `json:"campaigns"`
		Total     int64                       `json:"total"`
		Page      int                         `json:"page"`
		Limit     int                         `json:"limit"`
	}
	if err := c.get("/api/campaigns", q, &resp); err != nil {
		return "", nil, err
	}
	out := struct {
		Campaigns []campaignSummary `json:"campaigns"`
		Total     int64             `json:"total"`
		Page      int               `json:"page"`
		HasMore   bool              `json:"has_more"`
	}{Campaigns: make([]campaignSummary, len(resp.Campaigns)), Total: resp.Total, Page: resp.Page}
	out.HasMore = int64(resp.Page*resp.Limit) < resp.Total
	if len(resp.Campaigns) == 0 {
		return "No campaigns match.", out, nil
	}
	rows := make([][]string, len(resp.Campaigns))
	for i, cp := range resp.Campaigns {
		s := summarizeCampaign(cp)
		out.Campaigns[i] = s
		rows[i] = []string{s.Name, s.Status, s.Template, fmt.Sprint(s.TotalRecipients), fmt.Sprint(s.Sent),
			fmt.Sprint(s.Delivered), fmt.Sprint(s.Read), fmt.Sprint(s.Failed), s.ID}
	}
	text := fmt.Sprintf("%d of %d campaigns (page %d):\n\n", len(resp.Campaigns), resp.Total, resp.Page) +
		table([]string{"Name", "Status", "Template", "Recipients", "Sent", "Delivered", "Read", "Failed", "ID"}, rows)
	if out.HasMore {
		text += fmt.Sprintf("\nMore results: call again with page=%d.", resp.Page+1)
	}
	return text, out, nil
}

type campaignIDInput struct {
	CampaignID string `json:"campaign_id" jsonschema:"Campaign ID (UUID) from whatomate_list_campaigns"`
}

func (c *call) campaign(id string) (handlers.CampaignResponse, error) {
	var cp handlers.CampaignResponse
	err := c.get("/api/campaigns/"+pathID(id), nil, &cp)
	return cp, err
}

func campaignText(s campaignSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**: %s\n\n", s.Name, s.Status)
	fmt.Fprintf(&b, "- ID: %s\n- Template: %s\n- WhatsApp account: %s\n", s.ID, cell(s.Template), cell(s.WhatsAppAccount))
	fmt.Fprintf(&b, "- Recipients: %d; sent %d, delivered %d, read %d, failed %d\n", s.TotalRecipients, s.Sent, s.Delivered, s.Read, s.Failed)
	if s.ScheduledAt != "" {
		fmt.Fprintf(&b, "- Scheduled: %s\n", s.ScheduledAt)
	}
	if s.StartedAt != "" {
		fmt.Fprintf(&b, "- Started: %s\n", s.StartedAt)
	}
	if s.CompletedAt != "" {
		fmt.Fprintf(&b, "- Completed: %s\n", s.CompletedAt)
	}
	return b.String()
}

func getCampaign(c *call, in campaignIDInput) (string, any, error) {
	cp, err := c.campaign(in.CampaignID)
	if err != nil {
		return "", nil, err
	}
	s := summarizeCampaign(cp)
	return campaignText(s), s, nil
}

type listRecipientsInput struct {
	CampaignID string `json:"campaign_id" jsonschema:"Campaign ID (UUID)"`
	Status     string `json:"status,omitempty" jsonschema:"Only recipients with this status: pending, sent, delivered, read or failed"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum recipients to return, 1-500 (default 50)"`
}

type recipientSummary struct {
	PhoneNumber string `json:"phone_number"`
	Name        string `json:"name,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	MessageID   string `json:"message_id,omitempty"`
}

func (c *call) recipients(campaignID string) ([]models.BulkMessageRecipient, error) {
	var resp struct {
		Recipients []models.BulkMessageRecipient `json:"recipients"`
	}
	err := c.get("/api/campaigns/"+pathID(campaignID)+"/recipients", nil, &resp)
	return resp.Recipients, err
}

func listCampaignRecipients(c *call, in listRecipientsInput) (string, any, error) {
	all, err := c.recipients(in.CampaignID)
	if err != nil {
		return "", nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	counts := map[string]int{}
	var matched []recipientSummary
	total := 0
	for _, r := range all {
		counts[string(r.Status)]++
		if in.Status != "" && !strings.EqualFold(string(r.Status), in.Status) {
			continue
		}
		total++
		if len(matched) < limit {
			s := recipientSummary{PhoneNumber: r.PhoneNumber, Name: r.RecipientName, Status: string(r.Status), Error: r.ErrorMessage}
			if r.MessageID != nil {
				s.MessageID = r.MessageID.String()
			}
			matched = append(matched, s)
		}
	}
	out := struct {
		Recipients     []recipientSummary `json:"recipients"`
		Matching       int                `json:"matching"`
		CountsByStatus map[string]int     `json:"counts_by_status"`
	}{Recipients: matched, Matching: total, CountsByStatus: counts}
	if out.Recipients == nil {
		out.Recipients = []recipientSummary{}
	}

	statuses := make([]string, 0, len(counts))
	for _, st := range []string{"pending", "sent", "delivered", "read", "failed"} {
		if counts[st] > 0 {
			statuses = append(statuses, fmt.Sprintf("%s %d", st, counts[st]))
		}
	}
	text := fmt.Sprintf("%d recipients in total (%s).\n\n", len(all), cell(strings.Join(statuses, ", ")))
	if total == 0 {
		return text + "None match.", out, nil
	}
	rows := make([][]string, len(matched))
	for i, r := range matched {
		rows[i] = []string{r.PhoneNumber, r.Name, r.Status, truncate(r.Error, 100)}
	}
	text += fmt.Sprintf("Showing %d of %d matching:\n\n", len(matched), total) +
		table([]string{"Phone", "Name", "Status", "Error"}, rows)
	return text, out, nil
}

type createCampaignInput struct {
	Name            string `json:"name" jsonschema:"Campaign name"`
	TemplateID      string `json:"template_id" jsonschema:"ID (UUID) of an APPROVED template, from whatomate_list_templates"`
	WhatsAppAccount string `json:"whatsapp_account" jsonschema:"WhatsApp account name to send from; must be the template's account"`
}

func createCampaign(c *call, in createCampaignInput) (string, any, error) {
	body := handlers.CampaignRequest{Name: in.Name, TemplateID: in.TemplateID, WhatsAppAccount: in.WhatsAppAccount}
	var cp handlers.CampaignResponse
	if err := c.post("/api/campaigns", body, &cp); err != nil {
		return "", nil, err
	}
	c.audit(auditCampaigns, cp.ID.String(), models.AuditActionCreated, "create_campaign", map[string]any{"name": cp.Name})
	s := summarizeCampaign(cp)
	return fmt.Sprintf("Created draft campaign %q, ID %s. Next: add recipients with whatomate_add_campaign_recipients.", s.Name, s.ID), s, nil
}

type campaignRecipientInput struct {
	PhoneNumber    string            `json:"phone_number" jsonschema:"Phone in international format without + or spaces, e.g. 919876543210"`
	Name           string            `json:"name,omitempty" jsonschema:"Recipient name"`
	TemplateParams map[string]string `json:"template_params,omitempty" jsonschema:"This recipient's body variable values keyed by variable name"`
	HeaderParams   map[string]string `json:"header_params,omitempty" jsonschema:"This recipient's text header variable value keyed by its name"`
}

type addRecipientsInput struct {
	CampaignID string                   `json:"campaign_id" jsonschema:"Draft campaign ID (UUID)"`
	Recipients []campaignRecipientInput `json:"recipients" jsonschema:"Recipients to add"`
}

func addCampaignRecipients(c *call, in addRecipientsInput) (string, any, error) {
	if len(in.Recipients) == 0 {
		return "", nil, fmt.Errorf("recipients is empty")
	}
	if len(in.Recipients) > maxRecipientsPerCall {
		return "", nil, fmt.Errorf("%d recipients given; send at most %d per call and call again for the rest", len(in.Recipients), maxRecipientsPerCall)
	}
	recipients := make([]handlers.RecipientRequest, len(in.Recipients))
	for i, r := range in.Recipients {
		recipients[i] = handlers.RecipientRequest{
			PhoneNumber:    strings.TrimPrefix(strings.TrimSpace(r.PhoneNumber), "+"),
			RecipientName:  r.Name,
			TemplateParams: toAnyMap(r.TemplateParams),
			HeaderParams:   toAnyMap(r.HeaderParams),
		}
	}
	var resp struct {
		AddedCount int `json:"added_count"`
	}
	body := map[string]any{"recipients": recipients}
	if err := c.post("/api/campaigns/"+pathID(in.CampaignID)+"/recipients/import", body, &resp); err != nil {
		return "", nil, err
	}
	c.audit(auditCampaigns, in.CampaignID, models.AuditActionUpdated, "add_campaign_recipients", map[string]any{"added": resp.AddedCount})
	return fmt.Sprintf("Added %d recipients. When ready, call whatomate_start_campaign for a preview.", resp.AddedCount),
		map[string]any{"campaign_id": in.CampaignID, "added_count": resp.AddedCount}, nil
}

func toAnyMap(m map[string]string) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type startCampaignInput struct {
	CampaignID string `json:"campaign_id" jsonschema:"Campaign ID (UUID)"`
	Confirm    bool   `json:"confirm,omitempty" jsonschema:"Set true only after the user approved the preview; false or omitted returns the preview without sending"`
}

func startCampaign(c *call, in startCampaignInput) (string, any, error) {
	cp, err := c.campaign(in.CampaignID)
	if err != nil {
		return "", nil, err
	}
	s := summarizeCampaign(cp)
	switch cp.Status {
	case models.CampaignStatusDraft, models.CampaignStatusScheduled, models.CampaignStatusPaused:
	default:
		return "", nil, fmt.Errorf("campaign %q is %s and cannot be started; only draft, scheduled or paused campaigns can", s.Name, s.Status)
	}

	if !in.Confirm {
		all, err := c.recipients(in.CampaignID)
		if err != nil {
			return "", nil, err
		}
		pending := 0
		for _, r := range all {
			if r.Status == models.MessageStatusPending {
				pending++
			}
		}
		if pending == 0 {
			return "", nil, fmt.Errorf("campaign %q has no recipients waiting to be sent; add some with whatomate_add_campaign_recipients", s.Name)
		}
		preview := struct {
			campaignSummary
			WillSendTo int  `json:"will_send_to"`
			Preview    bool `json:"preview"`
		}{s, pending, true}
		text := fmt.Sprintf("PREVIEW, nothing has been sent.\n\nStarting campaign %q will send template %q from account %s to **%d recipients**. "+
			"Messages cannot be recalled once sent. Ask the user to confirm, then call whatomate_start_campaign again with confirm=true.",
			s.Name, s.Template, s.WhatsAppAccount, pending)
		return text, preview, nil
	}

	if err := c.post("/api/campaigns/"+pathID(in.CampaignID)+"/start", nil, nil); err != nil {
		return "", nil, err
	}
	c.audit(auditCampaigns, in.CampaignID, models.AuditActionUpdated, "start_campaign", map[string]any{"name": s.Name})
	return fmt.Sprintf("Campaign %q started. Track progress with whatomate_get_campaign.", s.Name),
		map[string]any{"campaign_id": s.ID, "status": string(models.CampaignStatusProcessing)}, nil
}

func pauseCampaign(c *call, in campaignIDInput) (string, any, error) {
	return changeCampaign(c, in.CampaignID, "pause", "paused")
}

func cancelCampaign(c *call, in campaignIDInput) (string, any, error) {
	return changeCampaign(c, in.CampaignID, "cancel", "cancelled")
}

func changeCampaign(c *call, campaignID, action, done string) (string, any, error) {
	if err := c.post("/api/campaigns/"+pathID(campaignID)+"/"+action, nil, nil); err != nil {
		return "", nil, err
	}
	c.audit(auditCampaigns, campaignID, models.AuditActionUpdated, action+"_campaign", nil)
	return fmt.Sprintf("Campaign %s %s.", campaignID, done), map[string]any{"campaign_id": campaignID, "status": done}, nil
}
