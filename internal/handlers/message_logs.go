package handlers

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const (
	// messageLogsPageSize is the fixed page size; there is no total count, the
	// page is walked with older/newer cursors instead.
	messageLogsPageSize = 50
	// messageLogsMaxAge is how far back the logs can be filtered.
	messageLogsMaxAge = 90 * 24 * time.Hour
	// messageLogsContentPreview caps content in list rows; the detail endpoint
	// returns the full text.
	messageLogsContentPreview = 200
	// messageLogsStatementTimeout bounds every logs query so an unusual filter
	// combination fails fast instead of tying up the database.
	messageLogsStatementTimeout = "5s"
	// pgQueryCanceled is Postgres' SQLSTATE for a statement_timeout cancel.
	pgQueryCanceled = "57014"
)

var (
	messageLogStatuses = map[string]bool{
		string(models.MessageStatusPending): true, string(models.MessageStatusSent): true,
		string(models.MessageStatusDelivered): true, string(models.MessageStatusRead): true,
		string(models.MessageStatusFailed): true, string(models.MessageStatusReceived): true,
	}
	messageLogDirections = map[string]bool{
		string(models.DirectionIncoming): true, string(models.DirectionOutgoing): true,
	}
	messageLogSources = map[string]bool{
		string(models.MessageSourceAPI): true, string(models.MessageSourceAgent): true,
		string(models.MessageSourceChatbot): true, string(models.MessageSourceCampaign): true,
		string(models.MessageSourceAutomation): true, string(models.MessageSourcePhoneApp): true,
		string(models.MessageSourceIncoming): true, string(models.MessageSourceCall): true,
	}
)

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

// messageLogCursor identifies the row a page starts after. Rows are ordered by
// (created_at, id) so rows sharing a timestamp page deterministically.
type messageLogCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func encodeMessageLogCursor(row messageLogRow) string {
	raw := row.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + row.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeMessageLogCursor(s string) (*messageLogCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil, errors.New("malformed cursor")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, err
	}
	rowID, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	return &messageLogCursor{CreatedAt: createdAt, ID: rowID}, nil
}

// parseMessageLogTime accepts a YYYY-MM-DD date (UTC) or an RFC3339 timestamp,
// so the UI can send exact local-day boundaries. endOfRange extends a bare
// date to the end of that day.
func parseMessageLogTime(s string, endOfRange bool) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		if endOfRange {
			return endOfDay(t), true
		}
		return t, true
	}
	return time.Time{}, false
}

// normalizePhoneFilter strips everything but digits, matching how contact
// phone numbers are stored ("+91 98765-43210" -> "919876543210").
func normalizePhoneFilter(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// ListMessageLogs returns one page of the organization's message delivery logs,
// newest first. from and to are required and limited to the last 90 days.
func (a *App) ListMessageLogs(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceMessageLogs, models.ActionRead)
	if err != nil {
		return nil
	}

	args := r.RequestCtx.QueryArgs()

	from, okFrom := parseMessageLogTime(string(args.Peek("from")), false)
	to, okTo := parseMessageLogTime(string(args.Peek("to")), true)
	if !okFrom || !okTo {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "from and to are required (YYYY-MM-DD or RFC3339)", nil, "")
	}
	if to.Before(from) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "from must be before to", nil, "")
	}
	// Allow a day of slack so a "last 90 days" range in any timezone passes.
	if from.Before(time.Now().Add(-messageLogsMaxAge - 24*time.Hour)) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Message logs are available for the last 90 days only", nil, "")
	}

	status := string(args.Peek("status"))
	direction := string(args.Peek("direction"))
	source := string(args.Peek("source"))
	if (status != "" && !messageLogStatuses[status]) ||
		(direction != "" && !messageLogDirections[direction]) ||
		(source != "" && !messageLogSources[source]) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid status, direction or source filter", nil, "")
	}

	var cursor *messageLogCursor
	if c := string(args.Peek("cursor")); c != "" {
		if cursor, err = decodeMessageLogCursor(c); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid cursor", nil, "")
		}
	}
	older := string(args.Peek("dir")) != "newer"

	account := string(args.Peek("account"))
	template := string(args.Peek("template"))

	var contactIDs []uuid.UUID
	if phone := normalizePhoneFilter(string(args.Peek("phone"))); phone != "" {
		// Resolve the contact first so the messages scan uses the per-contact
		// index. Unscoped: a deleted contact's messages are still logged.
		if err := a.DB.Unscoped().Model(&models.Contact{}).
			Where("organization_id = ? AND phone_number = ?", orgID, phone).
			Pluck("id", &contactIDs).Error; err != nil {
			a.Log.Error("Failed to resolve message log phone filter", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load message logs", nil, "")
		}
		if len(contactIDs) == 0 {
			return r.SendEnvelope(messageLogsEnvelope([]MessageLogResponse{}, "", ""))
		}
	}

	// Fetch one extra row to learn whether another page exists without counting.
	var rows []messageLogRow
	if err := a.withStatementTimeout(func(tx *gorm.DB) error {
		query := tx.Table("messages m").
			Select(messageLogSelect("LEFT(COALESCE(m.content, ''), ?)"), messageLogsContentPreview).
			// No deleted_at filter on contacts: logs keep a deleted contact's
			// name and number.
			Joins("LEFT JOIN contacts c ON c.id = m.contact_id").
			Where("m.organization_id = ? AND m.deleted_at IS NULL", orgID).
			Where("m.created_at >= ? AND m.created_at <= ?", from, to)

		if account != "" {
			query = query.Where("m.whats_app_account = ?", account)
		}
		if status != "" {
			query = query.Where("m.status = ?", status)
		}
		if direction != "" {
			query = query.Where("m.direction = ?", direction)
		}
		if source != "" {
			query = query.Where("m.source = ?", source)
		}
		if template != "" {
			query = query.Where("m.template_name = ?", template)
		}
		if len(contactIDs) > 0 {
			query = query.Where("m.contact_id IN ?", contactIDs)
		}

		switch {
		case cursor == nil:
			query = query.Order("m.created_at DESC, m.id DESC")
		case older:
			query = query.Where("(m.created_at, m.id) < (?, ?)", cursor.CreatedAt, cursor.ID).
				Order("m.created_at DESC, m.id DESC")
		default:
			query = query.Where("(m.created_at, m.id) > (?, ?)", cursor.CreatedAt, cursor.ID).
				Order("m.created_at ASC, m.id ASC")
		}

		return query.Limit(messageLogsPageSize + 1).Scan(&rows).Error
	}); err != nil {
		return a.sendMessageLogQueryError(r, err)
	}

	hasMore := len(rows) > messageLogsPageSize
	if hasMore {
		rows = rows[:messageLogsPageSize]
	}
	if !older {
		// Newer pages are fetched oldest-first; always return newest-first.
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}

	var nextCursor, prevCursor string
	if len(rows) > 0 {
		first, last := rows[0], rows[len(rows)-1]
		if older {
			if hasMore {
				nextCursor = encodeMessageLogCursor(last)
			}
			if cursor != nil {
				prevCursor = encodeMessageLogCursor(first)
			}
		} else {
			// We came from an older page, so older rows exist.
			nextCursor = encodeMessageLogCursor(last)
			if hasMore {
				prevCursor = encodeMessageLogCursor(first)
			}
		}
	}

	logs := a.messageLogRowsToResponse(orgID, rows)
	return r.SendEnvelope(messageLogsEnvelope(logs, nextCursor, prevCursor))
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

// withStatementTimeout runs fn in a transaction whose statements are cancelled
// by Postgres after messageLogsStatementTimeout.
func (a *App) withStatementTimeout(fn func(tx *gorm.DB) error) error {
	return a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL statement_timeout = '" + messageLogsStatementTimeout + "'").Error; err != nil {
			return err
		}
		return fn(tx)
	})
}

// sendMessageLogQueryError maps a timed-out query to a 422 asking the caller to
// narrow the filters, and anything else to a 500.
func (a *App) sendMessageLogQueryError(r *fastglue.Request, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgQueryCanceled {
		return r.SendErrorEnvelope(fasthttp.StatusUnprocessableEntity,
			"Query took too long. Narrow the date range or add filters.", nil, "")
	}
	a.Log.Error("Failed to load message logs", "error", err)
	return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load message logs", nil, "")
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

func messageLogsEnvelope(logs []MessageLogResponse, nextCursor, prevCursor string) map[string]any {
	return map[string]any{
		"logs":        logs,
		"next_cursor": nextCursor,
		"prev_cursor": prevCursor,
		"limit":       messageLogsPageSize,
	}
}
