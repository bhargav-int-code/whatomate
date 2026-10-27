package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/messagelogs"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const (
	// webhookResponseCaptureBytes is how much of a receiver's response body is
	// kept in the delivery log.
	webhookResponseCaptureBytes = 2 << 10
	// webhookResponseDrainBytes bounds how much more of a response is read and
	// discarded so the HTTP connection can be reused.
	webhookResponseDrainBytes = 64 << 10
	// webhookDeliveryStaleAfter is when a delivery still "processing" is
	// assumed lost (e.g. the server restarted mid-retry). Deliveries give up
	// after 2 minutes, so this leaves ample margin for in-flight ones on other
	// instances.
	webhookDeliveryStaleAfter = 10 * time.Minute
	// webhookDeliverySweepInterval is how often stale deliveries are closed out.
	webhookDeliverySweepInterval = 5 * time.Minute
	// webhookLogsPurgeMinAge is the youngest data the purge may delete.
	webhookLogsPurgeMinAge = 7 * 24 * time.Hour
	// webhookLogsPurgeBatch is how many rows each purge DELETE removes, so a
	// large purge never holds long locks.
	webhookLogsPurgeBatch = 5000
	// webhookLogsPurgeLockTTL bounds how long a crashed purge can block the next.
	webhookLogsPurgeLockTTL = 30 * time.Minute
)

var webhookDeliveryStatuses = map[string]bool{
	string(models.WebhookDeliveryProcessing): true,
	string(models.WebhookDeliverySuccess):    true,
	string(models.WebhookDeliveryFailed):     true,
}

// webhookLogSubject is implemented by event payloads that are about a message,
// contact or transfer, so their deliveries can be found by it.
type webhookLogSubject interface {
	webhookLogReference() (referenceID, contactPhone string)
}

func (d MessageEventData) webhookLogReference() (string, string) { return d.MessageID, d.ContactPhone }

func (d MessageFailedEventData) webhookLogReference() (string, string) {
	return d.MessageID, d.ContactPhone
}

func (d ContactEventData) webhookLogReference() (string, string) { return d.ContactID, d.ContactPhone }

func (d TransferEventData) webhookLogReference() (string, string) {
	return d.TransferID, d.ContactPhone
}

// sanitizeLogText makes arbitrary response text storable in a Postgres text
// column, which rejects NUL bytes and invalid UTF-8.
func sanitizeLogText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
}

// webhookFailureReason turns a delivery error into a readable reason.
func webhookFailureReason(err error) string {
	if err == nil {
		return ""
	}
	var whErr *WebhookError
	if errors.As(err, &whErr) {
		return fmt.Sprintf("HTTP %d %s", whErr.StatusCode, http.StatusText(whErr.StatusCode))
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "Timed out waiting for a response"
	}
	return sanitizeLogText(err.Error())
}

// startWebhookDelivery records a delivery about to be attempted and returns its
// ID. Logging must never stop a webhook going out, so a failed insert is only
// logged and uuid.Nil is returned, which the record functions ignore.
func (a *App) startWebhookDelivery(webhook models.Webhook, eventType string, data any, jsonData []byte, headers map[string]string) uuid.UUID {
	var referenceID, contactPhone string
	if subject, ok := data.(webhookLogSubject); ok {
		referenceID, contactPhone = subject.webhookLogReference()
	}

	recordedHeaders := make(models.JSONB, len(headers))
	for key, value := range headers {
		recordedHeaders[key] = value
	}

	delivery := models.WebhookDelivery{
		ID:             uuid.New(),
		OrganizationID: webhook.OrganizationID,
		WebhookID:      webhook.ID,
		WebhookName:    webhook.Name,
		Event:          eventType,
		ReferenceID:    referenceID,
		ContactPhone:   contactPhone,
		Status:         models.WebhookDeliveryProcessing,
		RequestURL:     webhook.URL,
		RequestHeaders: recordedHeaders,
		RequestBody:    string(jsonData),
		Attempts:       models.JSONBArray{},
	}
	if err := a.DB.Create(&delivery).Error; err != nil {
		a.Log.Error("Failed to record webhook delivery", "error", err, "webhook_id", webhook.ID, "event", eventType)
		return uuid.Nil
	}
	return delivery.ID
}

// recordWebhookAttempt appends one attempt to a delivery. final marks the
// delivery finished: success if this attempt succeeded, otherwise failed.
func (a *App) recordWebhookAttempt(deliveryID uuid.UUID, attempt int, result webhookAttemptResult, final bool) {
	if deliveryID == uuid.Nil {
		return
	}

	reason := webhookFailureReason(result.Err)
	now := time.Now()
	entry, err := json.Marshal([]map[string]any{{
		"attempt":     attempt,
		"at":          now.UTC(),
		"status_code": result.StatusCode,
		"duration_ms": result.Duration.Milliseconds(),
		"error":       reason,
	}})
	if err != nil {
		a.Log.Error("Failed to encode webhook attempt", "error", err, "delivery_id", deliveryID)
		return
	}

	status := models.WebhookDeliveryProcessing
	switch {
	case result.ok():
		status = models.WebhookDeliverySuccess
	case final:
		status = models.WebhookDeliveryFailed
	}

	updates := map[string]any{
		"attempts":        gorm.Expr("COALESCE(attempts, '[]'::jsonb) || ?::jsonb", string(entry)),
		"attempt_count":   attempt,
		"response_status": result.StatusCode,
		"response_body":   sanitizeLogText(result.ResponseBody),
		"failure_reason":  reason,
		"duration_ms":     result.Duration.Milliseconds(),
		"status":          status,
	}
	if status != models.WebhookDeliveryProcessing {
		updates["completed_at"] = now
	}
	if err := a.DB.Model(&models.WebhookDelivery{}).Where("id = ?", deliveryID).Updates(updates).Error; err != nil {
		a.Log.Error("Failed to record webhook attempt", "error", err, "delivery_id", deliveryID)
	}
}

// cancelWebhookDelivery closes out a delivery that stopped retrying because its
// context ended (dispatch timeout or shutdown).
func (a *App) cancelWebhookDelivery(deliveryID uuid.UUID, cause error) {
	if deliveryID == uuid.Nil {
		return
	}
	if err := a.DB.Model(&models.WebhookDelivery{}).
		Where("id = ? AND status = ?", deliveryID, models.WebhookDeliveryProcessing).
		Updates(map[string]any{
			"status":         models.WebhookDeliveryFailed,
			"failure_reason": "Delivery cancelled before it succeeded: " + webhookFailureReason(cause),
			"completed_at":   time.Now(),
		}).Error; err != nil {
		a.Log.Error("Failed to record cancelled webhook delivery", "error", err, "delivery_id", deliveryID)
	}
}

// FailStaleWebhookDeliveries marks deliveries stuck in "processing" for longer
// than webhookDeliveryStaleAfter as failed. Deliveries run in memory, so a
// restart mid-retry would otherwise leave them processing forever.
func (a *App) FailStaleWebhookDeliveries() int64 {
	result := a.DB.Model(&models.WebhookDelivery{}).
		Where("status = ? AND created_at < ?", models.WebhookDeliveryProcessing, time.Now().Add(-webhookDeliveryStaleAfter)).
		Updates(map[string]any{
			"status":         models.WebhookDeliveryFailed,
			"failure_reason": "Interrupted: the server stopped before the delivery finished",
			"completed_at":   time.Now(),
		})
	if result.Error != nil {
		a.Log.Error("Failed to close out stale webhook deliveries", "error", result.Error)
		return 0
	}
	if result.RowsAffected > 0 {
		a.Log.Warn("Marked interrupted webhook deliveries as failed", "count", result.RowsAffected)
	}
	return result.RowsAffected
}

// StartWebhookDeliverySweeper closes out stale deliveries now and then every
// webhookDeliverySweepInterval until ctx is cancelled.
func (a *App) StartWebhookDeliverySweeper(ctx context.Context) {
	a.FailStaleWebhookDeliveries()
	ticker := time.NewTicker(webhookDeliverySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.FailStaleWebhookDeliveries()
		}
	}
}

// WebhookLogResponse is one row of the webhook delivery logs. The request,
// response and attempts are only included by the detail endpoint.
type WebhookLogResponse struct {
	ID             uuid.UUID                    `json:"id"`
	CreatedAt      time.Time                    `json:"created_at"`
	WebhookID      uuid.UUID                    `json:"webhook_id"`
	WebhookName    string                       `json:"webhook_name"`
	Event          string                       `json:"event"`
	ReferenceID    string                       `json:"reference_id"`
	ContactPhone   string                       `json:"contact_phone"`
	Status         models.WebhookDeliveryStatus `json:"status"`
	AttemptCount   int                          `json:"attempt_count"`
	ResponseStatus int                          `json:"response_status"`
	FailureReason  string                       `json:"failure_reason"`
	DurationMs     int                          `json:"duration_ms"`
	CompletedAt    *time.Time                   `json:"completed_at"`
	// Detail endpoint only
	Request  *WebhookLogRequest      `json:"request,omitempty"`
	Response *WebhookLogResponseBody `json:"response,omitempty"`
	Attempts models.JSONBArray       `json:"attempts,omitempty"`
}

// WebhookLogRequest is the snapshot of what was sent
type WebhookLogRequest struct {
	Method  string          `json:"method"`
	URL     string          `json:"url"`
	Headers models.JSONB    `json:"headers"`
	Body    json.RawMessage `json:"body"`
}

// WebhookLogResponseBody is what the receiver answered on the last attempt
type WebhookLogResponseBody struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

// webhookLogListColumns excludes the snapshot columns, which can be large.
const webhookLogListColumns = `id, created_at, webhook_id, webhook_name, event, reference_id, contact_phone,
	status, attempt_count, response_status, failure_reason, duration_ms, completed_at`

func webhookLogKey(d models.WebhookDelivery) (time.Time, uuid.UUID) { return d.CreatedAt, d.ID }

func webhookDeliveryToResponse(d models.WebhookDelivery) WebhookLogResponse {
	return WebhookLogResponse{
		ID:             d.ID,
		CreatedAt:      d.CreatedAt,
		WebhookID:      d.WebhookID,
		WebhookName:    d.WebhookName,
		Event:          d.Event,
		ReferenceID:    d.ReferenceID,
		ContactPhone:   d.ContactPhone,
		Status:         d.Status,
		AttemptCount:   d.AttemptCount,
		ResponseStatus: d.ResponseStatus,
		FailureReason:  d.FailureReason,
		DurationMs:     d.DurationMs,
		CompletedAt:    d.CompletedAt,
	}
}

// ListWebhookLogs returns one page of webhook deliveries, newest first. from
// and to are required and limited to the last 90 days.
func (a *App) ListWebhookLogs(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceWebhookLogs, models.ActionRead)
	if err != nil {
		return nil
	}

	args := r.RequestCtx.QueryArgs()
	from, to, errMsg := parseLogRange(string(args.Peek("from")), string(args.Peek("to")))
	if errMsg != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, errMsg, nil, "")
	}

	status := string(args.Peek("status"))
	if status != "" && !webhookDeliveryStatuses[status] {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid status filter", nil, "")
	}
	var webhookID uuid.UUID
	if v := string(args.Peek("webhook_id")); v != "" {
		if webhookID, err = uuid.Parse(v); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid webhook_id", nil, "")
		}
	}
	page, err := parseLogPage(args)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid cursor", nil, "")
	}
	event := string(args.Peek("event"))
	search := strings.TrimSpace(string(args.Peek("search")))

	var rows []models.WebhookDelivery
	if err := a.withStatementTimeout(func(tx *gorm.DB) error {
		query := tx.Table("webhook_deliveries d").Select(webhookLogListColumns).
			Where("d.organization_id = ? AND d.created_at >= ? AND d.created_at <= ?", orgID, from, to)
		if webhookID != uuid.Nil {
			query = query.Where("d.webhook_id = ?", webhookID)
		}
		if status != "" {
			query = query.Where("d.status = ?", status)
		}
		if event != "" {
			query = query.Where("d.event = ?", event)
		}
		if search != "" {
			// Search matches the referenced message/contact/transfer ID, or the
			// customer's number in any format.
			if phone := messagelogs.NormalizePhone(search); phone != "" {
				query = query.Where("(d.reference_id = ? OR d.contact_phone = ?)", search, phone)
			} else {
				query = query.Where("d.reference_id = ?", search)
			}
		}
		return page.apply(query, "d").Scan(&rows).Error
	}); err != nil {
		return a.sendLogQueryError(r, err, "webhook logs")
	}

	rows, nextCursor, prevCursor := finishLogPage(page, rows, webhookLogKey)
	logs := make([]WebhookLogResponse, len(rows))
	for i, row := range rows {
		logs[i] = webhookDeliveryToResponse(row)
	}
	return r.SendEnvelope(logsEnvelope("logs", logs, nextCursor, prevCursor))
}

// GetWebhookLog returns one delivery with its request snapshot, the last
// response and every attempt.
func (a *App) GetWebhookLog(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceWebhookLogs, models.ActionRead)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "webhook log")
	if err != nil {
		return nil
	}

	delivery, err := findByIDAndOrg[models.WebhookDelivery](a.DB, r, id, orgID, "Webhook log")
	if err != nil {
		return nil
	}

	resp := webhookDeliveryToResponse(*delivery)
	body := json.RawMessage(delivery.RequestBody)
	if !json.Valid(body) {
		// Never expected (we always send JSON), but keep the response valid.
		body, _ = json.Marshal(delivery.RequestBody)
	}
	resp.Request = &WebhookLogRequest{
		Method:  http.MethodPost,
		URL:     delivery.RequestURL,
		Headers: delivery.RequestHeaders,
		Body:    body,
	}
	if delivery.ResponseStatus != 0 || delivery.ResponseBody != "" {
		resp.Response = &WebhookLogResponseBody{StatusCode: delivery.ResponseStatus, Body: delivery.ResponseBody}
	}
	resp.Attempts = delivery.Attempts
	if resp.Attempts == nil {
		resp.Attempts = models.JSONBArray{}
	}
	return r.SendEnvelope(resp)
}

// PurgeWebhookLogsRequest is the body of POST /api/webhook-logs/purge. Without
// confirm it only reports how many deliveries match.
type PurgeWebhookLogsRequest struct {
	Before    string `json:"before"`     // YYYY-MM-DD or RFC3339; deletes deliveries created before this
	WebhookID string `json:"webhook_id"` // optional: only this webhook's deliveries
	Confirm   bool   `json:"confirm"`
}

// PurgeWebhookLogs deletes deliveries older than a cutoff, which must be at
// least 7 days ago. The deletion runs in the background in batches; only one
// purge per organization runs at a time.
func (a *App) PurgeWebhookLogs(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceWebhookLogs, models.ActionDelete)
	if err != nil {
		return nil
	}

	var req PurgeWebhookLogsRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	before, ok := parseLogTime(req.Before, false)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "before is required (YYYY-MM-DD or RFC3339)", nil, "")
	}
	if before.After(time.Now().Add(-webhookLogsPurgeMinAge)) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Only webhook logs older than 7 days can be purged", nil, "")
	}
	var webhookID uuid.UUID
	if req.WebhookID != "" {
		if webhookID, err = uuid.Parse(req.WebhookID); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid webhook_id", nil, "")
		}
	}

	scope := func(db *gorm.DB) *gorm.DB {
		db = db.Where("organization_id = ? AND created_at < ?", orgID, before)
		if webhookID != uuid.Nil {
			db = db.Where("webhook_id = ?", webhookID)
		}
		return db
	}

	var matching int64
	if err := a.withStatementTimeout(func(tx *gorm.DB) error {
		return scope(tx.Model(&models.WebhookDelivery{})).Count(&matching).Error
	}); err != nil {
		return a.sendLogQueryError(r, err, "webhook logs")
	}
	if !req.Confirm || matching == 0 {
		return r.SendEnvelope(map[string]any{"matching": matching, "started": false})
	}

	lockKey := "webhook_logs:purge:" + orgID.String()
	acquired, err := a.Redis.SetNX(context.Background(), lockKey, userID.String(), webhookLogsPurgeLockTTL).Result()
	if err != nil {
		a.Log.Error("Failed to acquire webhook logs purge lock", "error", err, "org_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start purge", nil, "")
	}
	if !acquired {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "A webhook logs purge is already running", nil, "")
	}

	a.logAudit(orgID, userID, models.ResourceWebhookLogs, uuid.Nil, models.AuditActionDeleted,
		map[string]any{"before": before.Format(time.RFC3339), "webhook_id": req.WebhookID, "deliveries": matching}, nil)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer a.Redis.Del(context.Background(), lockKey)
		deleted := a.purgeWebhookDeliveries(scope)
		a.Log.Info("Purged webhook logs", "org_id", orgID, "before", before, "webhook_id", req.WebhookID, "deleted", deleted)
	}()

	return r.SendEnvelope(map[string]any{"matching": matching, "started": true})
}

// purgeWebhookDeliveries deletes the rows selected by scope in batches and
// returns how many were deleted.
func (a *App) purgeWebhookDeliveries(scope func(*gorm.DB) *gorm.DB) int64 {
	var total int64
	for {
		batch := scope(a.DB.Model(&models.WebhookDelivery{})).Select("id").Limit(webhookLogsPurgeBatch)
		result := a.DB.Where("id IN (?)", batch).Delete(&models.WebhookDelivery{})
		if result.Error != nil {
			a.Log.Error("Webhook logs purge failed", "error", result.Error, "deleted_so_far", total)
			return total
		}
		total += result.RowsAffected
		if result.RowsAffected < webhookLogsPurgeBatch {
			return total
		}
	}
}
