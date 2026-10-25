package handlers

import (
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/messagelogs"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// messageLogsContentPreview caps content in list rows; the detail endpoint
// returns the full text.
const messageLogsContentPreview = 200

// MessageLogResponse is one row of the message delivery logs
type MessageLogResponse struct {
	ID                uuid.UUID            `json:"id"`
	CreatedAt         time.Time            `json:"created_at"`
	Direction         models.Direction     `json:"direction"`
	Source            models.MessageSource `json:"source"`
	Status            models.MessageStatus `json:"status"`
	SentAt            *time.Time           `json:"sent_at"`
	DeliveredAt       *time.Time           `json:"delivered_at"`
	ReadAt            *time.Time           `json:"read_at"`
	ContactID         uuid.UUID            `json:"contact_id"`
	ContactName       string               `json:"contact_name"`
	ContactPhone      string               `json:"contact_phone"`
	WhatsAppAccount   string               `json:"whatsapp_account"`
	MessageType       models.MessageType   `json:"message_type"`
	TemplateName      string               `json:"template_name"`
	Content           string               `json:"content"`
	ErrorMessage      string               `json:"error_message"`
	PricingCategory   string               `json:"pricing_category"`
	CampaignID        string               `json:"campaign_id"`
	CampaignName      string               `json:"campaign_name"`
	WhatsAppMessageID string               `json:"whatsapp_message_id"`
	// Detail endpoint only
	SentByUserID   *uuid.UUID `json:"sent_by_user_id,omitempty"`
	SentByUserName string     `json:"sent_by_user_name,omitempty"`
}

// messageLogRow is the scan target for the logs query
type messageLogRow struct {
	ID                uuid.UUID
	CreatedAt         time.Time
	Direction         models.Direction
	Source            models.MessageSource
	Status            models.MessageStatus
	SentAt            *time.Time
	DeliveredAt       *time.Time
	ReadAt            *time.Time
	ContactID         uuid.UUID
	ContactName       string
	ContactPhone      string
	WhatsAppAccount   string
	MessageType       models.MessageType
	TemplateName      string
	Content           string
	ErrorMessage      string
	PricingCategory   string
	CampaignID        string
	WhatsAppMessageID string
	SentByUserID      *uuid.UUID
	SentByUserName    string
}

// messageLogSelect lists the columns for both endpoints. contentExpr lets the
// list endpoint truncate content in SQL so long messages aren't transferred.
func messageLogSelect(contentExpr string) string {
	return `m.id, m.created_at, m.direction, m.source, m.status,
		m.sent_at, m.delivered_at, m.read_at, m.contact_id,
		COALESCE(c.profile_name, '') AS contact_name,
		COALESCE(c.phone_number, '') AS contact_phone,
		m.whats_app_account AS whats_app_account, m.message_type,
		COALESCE(m.template_name, '') AS template_name,
		` + contentExpr + ` AS content,
		COALESCE(m.error_message, '') AS error_message,
		COALESCE(m.pricing_category, '') AS pricing_category,
		COALESCE(m.metadata->>'campaign_id', '') AS campaign_id,
		COALESCE(m.whats_app_message_id, '') AS whats_app_message_id`
}

func messageLogKey(row messageLogRow) (time.Time, uuid.UUID) { return row.CreatedAt, row.ID }

// messageLogFiltersFromArgs reads the message log filters from query params.
// On failure it returns a message suitable for a 400 response.
func messageLogFiltersFromArgs(args *fasthttp.Args) (messagelogs.Filters, string) {
	from, to, errMsg := parseLogRange(string(args.Peek("from")), string(args.Peek("to")))
	if errMsg != "" {
		return messagelogs.Filters{}, errMsg
	}
	filters := messagelogs.Filters{
		From:      from,
		To:        to,
		Account:   string(args.Peek("account")),
		Status:    string(args.Peek("status")),
		Direction: string(args.Peek("direction")),
		Source:    string(args.Peek("source")),
		Template:  string(args.Peek("template")),
		Phone:     messagelogs.NormalizePhone(string(args.Peek("phone"))),
	}
	if err := filters.Validate(); err != nil {
		return filters, "Invalid status, direction or source filter"
	}
	return filters, ""
}

// ListMessageLogs returns one page of the organization's message delivery logs,
// newest first. from and to are required and limited to the last 90 days.
func (a *App) ListMessageLogs(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceMessageLogs, models.ActionRead)
	if err != nil {
		return nil
	}

	args := r.RequestCtx.QueryArgs()

	filters, errMsg := messageLogFiltersFromArgs(args)
	if errMsg != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, errMsg, nil, "")
	}

	page, err := parseLogPage(args)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid cursor", nil, "")
	}

	var contactIDs []uuid.UUID
	if filters.Phone != "" {
		// Resolve the contact first so the messages scan uses the per-contact
		// index.
		if contactIDs, err = messagelogs.ResolveContacts(a.DB, orgID, filters.Phone); err != nil {
			a.Log.Error("Failed to resolve message log phone filter", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load message logs", nil, "")
		}
		if len(contactIDs) == 0 {
			return r.SendEnvelope(logsEnvelope("logs", []MessageLogResponse{}, "", ""))
		}
	}

	// Fetch one extra row to learn whether another page exists without counting.
	var rows []messageLogRow
	if err := a.withStatementTimeout(func(tx *gorm.DB) error {
		query := tx.Table("messages m").
			Select(messageLogSelect("LEFT(COALESCE(m.content, ''), ?)"), messageLogsContentPreview).
			// No deleted_at filter on contacts: logs keep a deleted contact's
			// name and number.
			Joins("LEFT JOIN contacts c ON c.id = m.contact_id")
		return page.apply(filters.Apply(query, orgID, contactIDs), "m").Scan(&rows).Error
	}); err != nil {
		return a.sendLogQueryError(r, err, "message logs")
	}

	rows, nextCursor, prevCursor := finishLogPage(page, rows, messageLogKey)
	logs := a.messageLogRowsToResponse(orgID, rows)
	return r.SendEnvelope(logsEnvelope("logs", logs, nextCursor, prevCursor))
}

// GetMessageLog returns a single message log entry with the full content and
// the name of the user who sent it.
func (a *App) GetMessageLog(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceMessageLogs, models.ActionRead)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "message")
	if err != nil {
		return nil
	}

	var rows []messageLogRow
	if err := a.DB.Table("messages m").
		Select(messageLogSelect("COALESCE(m.content, '')")+
			`, m.sent_by_user_id, COALESCE(u.full_name, '') AS sent_by_user_name`).
		Joins("LEFT JOIN contacts c ON c.id = m.contact_id").
		Joins("LEFT JOIN users u ON u.id = m.sent_by_user_id").
		Where("m.id = ? AND m.organization_id = ? AND m.deleted_at IS NULL", id, orgID).
		Limit(1).Scan(&rows).Error; err != nil {
		a.Log.Error("Failed to load message log", "error", err, "message_id", id)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load message log", nil, "")
	}
	if len(rows) == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Message not found", nil, "")
	}

	return r.SendEnvelope(a.messageLogRowsToResponse(orgID, rows)[0])
}

// messageLogRowsToResponse converts scanned rows, resolving campaign names in
// one query and applying the organization's phone masking setting.
func (a *App) messageLogRowsToResponse(orgID uuid.UUID, rows []messageLogRow) []MessageLogResponse {
	campaignNames := a.campaignNamesForLogs(orgID, rows)
	mask := a.ShouldMaskPhoneNumbers(orgID)

	out := make([]MessageLogResponse, len(rows))
	for i, row := range rows {
		name, phone := row.ContactName, row.ContactPhone
		if mask {
			name = utils.MaskIfPhoneNumber(name)
			phone = utils.MaskPhoneNumber(phone)
		}
		out[i] = MessageLogResponse{
			ID:                row.ID,
			CreatedAt:         row.CreatedAt,
			Direction:         row.Direction,
			Source:            row.Source,
			Status:            row.Status,
			SentAt:            row.SentAt,
			DeliveredAt:       row.DeliveredAt,
			ReadAt:            row.ReadAt,
			ContactID:         row.ContactID,
			ContactName:       name,
			ContactPhone:      phone,
			WhatsAppAccount:   row.WhatsAppAccount,
			MessageType:       row.MessageType,
			TemplateName:      row.TemplateName,
			Content:           row.Content,
			ErrorMessage:      row.ErrorMessage,
			PricingCategory:   row.PricingCategory,
			CampaignID:        row.CampaignID,
			CampaignName:      campaignNames[row.CampaignID],
			WhatsAppMessageID: row.WhatsAppMessageID,
			SentByUserID:      row.SentByUserID,
			SentByUserName:    row.SentByUserName,
		}
	}
	return out
}

// campaignNamesForLogs looks up the names of the campaigns referenced by a page
// of rows. A separate query (rather than a join on the metadata JSON) keeps a
// malformed campaign_id from failing the whole page.
func (a *App) campaignNamesForLogs(orgID uuid.UUID, rows []messageLogRow) map[string]string {
	ids := make([]uuid.UUID, 0)
	seen := make(map[uuid.UUID]bool)
	for _, row := range rows {
		if id, err := uuid.Parse(row.CampaignID); err == nil && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	names := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return names
	}

	var campaigns []models.BulkMessageCampaign
	if err := a.DB.Unscoped().Select("id", "name").
		Where("organization_id = ? AND id IN ?", orgID, ids).
		Find(&campaigns).Error; err != nil {
		a.Log.Warn("Failed to load campaign names for message logs", "error", err)
		return names
	}
	for _, c := range campaigns {
		names[c.ID.String()] = c.Name
	}
	return names
}
