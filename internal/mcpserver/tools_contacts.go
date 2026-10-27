package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
)

func init() {
	defineTool(ScopeMessagesRead, "", &mcp.Tool{
		Name:        "whatomate_get_account_info",
		Description: "Show who this connection acts as: the user, organization, role, granted scopes and role permissions. Call this first when unsure what you are allowed to do.",
		Annotations: readOnly("Get account info"),
	}, getAccountInfo)

	defineTool(ScopeMessagesRead, "accounts:read", &mcp.Tool{
		Name:        "whatomate_list_whatsapp_accounts",
		Description: "List the organization's WhatsApp Business phone numbers (accounts). The account name is what other tools take as whatsapp_account.",
		Annotations: readOnly("List WhatsApp accounts"),
	}, listWhatsAppAccounts)

	defineTool(ScopeMessagesRead, "contacts:read", &mcp.Tool{
		Name:        "whatomate_search_contacts",
		Description: "Search contacts by phone number or name, optionally filtered by tags. Returns each contact's ID, which other tools need, and whether its 24-hour service window is open (free-form messages allowed).",
		Annotations: readOnly("Search contacts"),
	}, searchContacts)

	defineTool(ScopeMessagesRead, "contacts:read", &mcp.Tool{
		Name:        "whatomate_get_contact",
		Description: "Get one contact's details: phone, name, tags, metadata, assigned agent, last message and service window status.",
		Annotations: readOnly("Get contact"),
	}, getContact)

	defineTool(ScopeMessagesRead, "chat:read", &mcp.Tool{
		Name:        "whatomate_get_conversation",
		Description: "Read a contact's WhatsApp conversation, oldest to newest. Returns the most recent messages; pass before_message_id to page further back. Reading does not mark messages as read.",
		Annotations: readOnly("Get conversation"),
	}, getConversation)

	defineTool(ScopeMessagesSend, "contacts:write", &mcp.Tool{
		Name:        "whatomate_create_contact",
		Description: "Create a contact (or restore a deleted one) so it can be messaged, tagged or added to campaigns.",
		Annotations: write("Create contact", false),
	}, createContact)

	defineTool(ScopeMessagesSend, "contacts:write", &mcp.Tool{
		Name:        "whatomate_update_contact_tags",
		Description: "Replace a contact's tags with the given list. To add or remove one tag, read the current tags with whatomate_get_contact first and send the full new list.",
		Annotations: write("Update contact tags", true),
	}, updateContactTags)
}

type noInput struct{}

type accountInfo struct {
	UserID       string   `json:"user_id"`
	Name         string   `json:"name"`
	Email        string   `json:"email"`
	Organization string   `json:"organization"`
	Role         string   `json:"role"`
	IsSuperAdmin bool     `json:"is_super_admin"`
	Scopes       []string `json:"scopes"`
	Permissions  []string `json:"permissions"`
}

func getAccountInfo(c *call, _ noInput) (string, any, error) {
	var me handlers.UserResponse
	if err := c.get("/api/me", nil, &me); err != nil {
		return "", nil, err
	}
	var org handlers.OrganizationResponse
	if err := c.get("/api/organizations/current", nil, &org); err != nil {
		return "", nil, err
	}

	info := accountInfo{
		UserID:       me.ID.String(),
		Name:         me.FullName,
		Email:        me.Email,
		Organization: org.Name,
		IsSuperAdmin: me.IsSuperAdmin,
		Scopes:       scopeNames(c.p.Scopes),
	}
	if me.Role != nil {
		info.Role = me.Role.Name
		for _, perm := range me.Role.Permissions {
			info.Permissions = append(info.Permissions, perm.Resource+":"+perm.Action)
		}
		sort.Strings(info.Permissions)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Connected as **%s** (%s) in organization **%s**.\n\n", info.Name, info.Email, info.Organization)
	fmt.Fprintf(&b, "- Role: %s\n- Scopes: %s\n", cell(info.Role), strings.Join(info.Scopes, ", "))
	if info.IsSuperAdmin {
		b.WriteString("- Super admin: all permissions\n")
	} else {
		fmt.Fprintf(&b, "- Permissions: %s\n", cell(strings.Join(info.Permissions, ", ")))
	}
	return b.String(), info, nil
}

type whatsAppAccount struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	PhoneNumber       string `json:"phone_number,omitempty"`
	DisplayName       string `json:"display_name,omitempty"`
	Status            string `json:"status"`
	IsDefaultOutgoing bool   `json:"is_default_outgoing"`
}

func listWhatsAppAccounts(c *call, _ noInput) (string, any, error) {
	var resp struct {
		Accounts []whatsAppAccount `json:"accounts"`
	}
	if err := c.get("/api/accounts", nil, &resp); err != nil {
		return "", nil, err
	}
	if len(resp.Accounts) == 0 {
		return "No WhatsApp accounts are connected. An admin can add one under Settings → Accounts.", resp, nil
	}
	rows := make([][]string, len(resp.Accounts))
	for i, a := range resp.Accounts {
		def := ""
		if a.IsDefaultOutgoing {
			def = "yes"
		}
		rows[i] = []string{a.Name, a.PhoneNumber, a.DisplayName, a.Status, def}
	}
	return table([]string{"Account name", "Phone", "Display name", "Status", "Default for sending"}, rows), resp, nil
}

type searchContactsInput struct {
	Query string   `json:"query,omitempty" jsonschema:"Part of a phone number or name to search for"`
	Tags  []string `json:"tags,omitempty" jsonschema:"Only contacts having any of these tags"`
	Page  int      `json:"page,omitempty" jsonschema:"Page number, starting at 1"`
	Limit int      `json:"limit,omitempty" jsonschema:"Contacts per page, 1-100 (default 20)"`
}

// contactSummary is the subset of a contact the tools return.
type contactSummary struct {
	ID                string     `json:"id"`
	PhoneNumber       string     `json:"phone_number"`
	Name              string     `json:"name"`
	Tags              []string   `json:"tags"`
	WhatsAppAccount   string     `json:"whatsapp_account,omitempty"`
	LastMessageAt     *time.Time `json:"last_message_at,omitempty"`
	LastMessage       string     `json:"last_message,omitempty"`
	UnreadCount       int        `json:"unread_count"`
	ServiceWindowOpen bool       `json:"service_window_open"`
	MarketingOptOut   bool       `json:"marketing_opt_out"`
	Metadata          any        `json:"metadata,omitempty"`
	AssignedUserID    string     `json:"assigned_user_id,omitempty"`
}

func summarizeContact(ct handlers.ContactResponse) contactSummary {
	s := contactSummary{
		ID:                ct.ID.String(),
		PhoneNumber:       ct.PhoneNumber,
		Name:              contactName(ct),
		Tags:              ct.Tags,
		WhatsAppAccount:   ct.WhatsAppAccount,
		LastMessageAt:     ct.LastMessageAt,
		LastMessage:       ct.LastMessagePreview,
		UnreadCount:       ct.UnreadCount,
		ServiceWindowOpen: ct.ServiceWindowOpen,
		MarketingOptOut:   ct.MarketingOptOut,
		Metadata:          ct.Metadata,
	}
	if ct.AssignedUserID != nil {
		s.AssignedUserID = ct.AssignedUserID.String()
	}
	return s
}

func contactName(ct handlers.ContactResponse) string {
	if ct.Name != "" {
		return ct.Name
	}
	return ct.ProfileName
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func searchContacts(c *call, in searchContactsInput) (string, any, error) {
	q := pageArgs(in.Page, in.Limit, 20, 100)
	setIf(q, "search", strings.TrimPrefix(strings.TrimSpace(in.Query), "+"))
	setIf(q, "tags", strings.Join(in.Tags, ","))

	var resp struct {
		Contacts []handlers.ContactResponse `json:"contacts"`
		Total    int64                      `json:"total"`
		Page     int                        `json:"page"`
		Limit    int                        `json:"limit"`
	}
	if err := c.get("/api/contacts", q, &resp); err != nil {
		return "", nil, err
	}

	out := struct {
		Contacts []contactSummary `json:"contacts"`
		Total    int64            `json:"total"`
		Page     int              `json:"page"`
		HasMore  bool             `json:"has_more"`
	}{Contacts: make([]contactSummary, len(resp.Contacts)), Total: resp.Total, Page: resp.Page}
	out.HasMore = int64(resp.Page*resp.Limit) < resp.Total

	if len(resp.Contacts) == 0 {
		return "No contacts match.", out, nil
	}
	rows := make([][]string, len(resp.Contacts))
	for i, ct := range resp.Contacts {
		out.Contacts[i] = summarizeContact(ct)
		rows[i] = []string{contactName(ct), ct.PhoneNumber, strings.Join(ct.Tags, ", "),
			fmtTime(ct.LastMessageAt), yesNo(ct.ServiceWindowOpen), ct.ID.String()}
	}
	text := fmt.Sprintf("%d of %d contacts (page %d):\n\n", len(resp.Contacts), resp.Total, resp.Page) +
		table([]string{"Name", "Phone", "Tags", "Last message", "Window open", "ID"}, rows)
	if out.HasMore {
		text += fmt.Sprintf("\nMore results: call again with page=%d.", resp.Page+1)
	}
	return text, out, nil
}

type contactIDInput struct {
	ContactID string `json:"contact_id" jsonschema:"Contact ID (UUID) from whatomate_search_contacts"`
}

func getContact(c *call, in contactIDInput) (string, any, error) {
	var ct handlers.ContactResponse
	if err := c.get("/api/contacts/"+pathID(in.ContactID), nil, &ct); err != nil {
		return "", nil, err
	}
	s := summarizeContact(ct)

	var b strings.Builder
	fmt.Fprintf(&b, "**%s** (%s)\n\n", cell(s.Name), ct.PhoneNumber)
	fmt.Fprintf(&b, "- ID: %s\n- Tags: %s\n- WhatsApp account: %s\n", s.ID, cell(strings.Join(ct.Tags, ", ")), cell(ct.WhatsAppAccount))
	fmt.Fprintf(&b, "- Last message: %s (%s)\n- Unread: %d\n", fmtTime(ct.LastMessageAt), cell(truncate(ct.LastMessagePreview, 120)), ct.UnreadCount)
	if ct.ServiceWindowOpen {
		b.WriteString("- Service window: open, free-form messages allowed\n")
	} else {
		b.WriteString("- Service window: closed, only templates can be sent\n")
	}
	if ct.MarketingOptOut {
		b.WriteString("- Opted out of marketing messages\n")
	}
	if m, ok := ct.Metadata.(map[string]any); ok && len(m) > 0 {
		meta, _ := json.Marshal(m)
		fmt.Fprintf(&b, "- Metadata: %s\n", truncate(string(meta), 500))
	}
	return b.String(), s, nil
}

type getConversationInput struct {
	ContactID       string `json:"contact_id" jsonschema:"Contact ID (UUID) from whatomate_search_contacts"`
	Limit           int    `json:"limit,omitempty" jsonschema:"Number of messages, 1-100 (default 30)"`
	BeforeMessageID string `json:"before_message_id,omitempty" jsonschema:"Return messages older than this message ID, to page back"`
}

type conversationMessage struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Direction string    `json:"direction"`
	Type      string    `json:"type"`
	Text      string    `json:"text"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
}

func getConversation(c *call, in getConversationInput) (string, any, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := url.Values{"limit": {fmt.Sprint(limit)}}
	setIf(q, "before_id", strings.TrimSpace(in.BeforeMessageID))

	var resp struct {
		Messages []handlers.MessageResponse `json:"messages"`
		Total    int64                      `json:"total"`
	}
	if err := c.get("/api/contacts/"+pathID(in.ContactID)+"/messages", q, &resp); err != nil {
		return "", nil, err
	}

	out := struct {
		Messages []conversationMessage `json:"messages"`
		Total    int64                 `json:"total"`
		HasOlder bool                  `json:"has_older"`
	}{Messages: make([]conversationMessage, len(resp.Messages)), Total: resp.Total}
	if len(resp.Messages) == 0 {
		return "No messages in this conversation.", out, nil
	}
	out.HasOlder = len(resp.Messages) == limit

	var b strings.Builder
	for i, m := range resp.Messages {
		cm := conversationMessage{
			ID:        m.ID.String(),
			At:        m.CreatedAt,
			Direction: string(m.Direction),
			Type:      string(m.MessageType),
			Text:      messageText(m),
			Status:    string(m.Status),
			Error:     m.Error,
		}
		out.Messages[i] = cm
		who := "Contact"
		if m.Direction == "outgoing" {
			who = "Business"
		}
		fmt.Fprintf(&b, "[%s] %s (%s, %s): %s", fmtTime(&m.CreatedAt), who, cm.Type, cm.Status, truncate(cm.Text, 1000))
		if cm.Error != "" {
			fmt.Fprintf(&b, " — error: %s", cm.Error)
		}
		b.WriteString("\n")
	}
	if out.HasOlder {
		fmt.Fprintf(&b, "\nOlder messages exist: call again with before_message_id=%s.", resp.Messages[0].ID)
	}
	return b.String(), out, nil
}

// messageText is a readable version of a message's content.
func messageText(m handlers.MessageResponse) string {
	switch v := m.Content.(type) {
	case string:
		return v
	case map[string]any:
		if body, ok := v["body"].(string); ok && body != "" {
			return body
		}
		if len(v) == 0 {
			break
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	if m.MediaFilename != "" {
		return "[" + string(m.MessageType) + ": " + m.MediaFilename + "]"
	}
	return "[" + string(m.MessageType) + "]"
}

type createContactInput struct {
	PhoneNumber     string   `json:"phone_number" jsonschema:"Phone number in international format without + or spaces, e.g. 919876543210"`
	Name            string   `json:"name,omitempty" jsonschema:"Contact name"`
	Tags            []string `json:"tags,omitempty" jsonschema:"Tags to set"`
	WhatsAppAccount string   `json:"whatsapp_account,omitempty" jsonschema:"WhatsApp account name to message this contact from (default: the organization's default)"`
}

func createContact(c *call, in createContactInput) (string, any, error) {
	body := handlers.CreateContactRequest{
		PhoneNumber:     strings.TrimPrefix(strings.TrimSpace(in.PhoneNumber), "+"),
		ProfileName:     in.Name,
		WhatsAppAccount: in.WhatsAppAccount,
		Tags:            in.Tags,
	}
	var ct handlers.ContactResponse
	if err := c.post("/api/contacts", body, &ct); err != nil {
		return "", nil, err
	}
	c.audit(auditContacts, ct.ID.String(), models.AuditActionCreated, "create_contact", map[string]any{"phone_number": ct.PhoneNumber})
	return fmt.Sprintf("Created contact %s (%s), ID %s.", cell(contactName(ct)), ct.PhoneNumber, ct.ID), summarizeContact(ct), nil
}

type updateContactTagsInput struct {
	ContactID string   `json:"contact_id" jsonschema:"Contact ID (UUID)"`
	Tags      []string `json:"tags" jsonschema:"The complete new list of tags; an empty list removes all tags"`
}

func updateContactTags(c *call, in updateContactTagsInput) (string, any, error) {
	tags := in.Tags
	if tags == nil {
		tags = []string{}
	}
	var resp struct {
		Tags []string `json:"tags"`
	}
	if err := c.put("/api/contacts/"+pathID(in.ContactID)+"/tags", handlers.UpdateContactTagsRequest{Tags: tags}, &resp); err != nil {
		return "", nil, err
	}
	c.audit(auditContacts, in.ContactID, models.AuditActionUpdated, "update_contact_tags", map[string]any{"tags": strings.Join(resp.Tags, ", ")})
	text := "Removed all tags."
	if len(resp.Tags) > 0 {
		text = "Tags are now: " + strings.Join(resp.Tags, ", ") + "."
	}
	return text, map[string]any{"contact_id": in.ContactID, "tags": resp.Tags}, nil
}
