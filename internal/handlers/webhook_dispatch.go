package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

// OutboundWebhookPayload represents the structure sent to external webhook endpoints
type OutboundWebhookPayload struct {
	Event     string    `json:"event"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data"`
}

// MessageEventData represents data for message events
type MessageEventData struct {
	MessageID        string             `json:"message_id"`
	ContactID        string             `json:"contact_id"`
	ContactPhone     string             `json:"contact_phone"`
	ContactName      string             `json:"contact_name"`
	MessageType      models.MessageType `json:"message_type"`
	Content          string             `json:"content"`
	MediaURL         string             `json:"media_url,omitempty"` // base-path-relative path to the media endpoint, not the storage path
	MediaMimeType    string             `json:"media_mime_type,omitempty"`
	MediaFilename    string             `json:"media_filename,omitempty"`
	ReplyToMessageID string             `json:"reply_to_message_id,omitempty"`
	WhatsAppAccount  string             `json:"whatsapp_account"`
	Direction        models.Direction   `json:"direction,omitempty"`
	SentByUserID     string             `json:"sent_by_user_id,omitempty"`
}

// Message failure sources reported in MessageFailedEventData.Source
const (
	// MessageFailureSourceSendError means the send request to Meta was rejected
	// (or never completed), so the message has no WhatsApp message ID.
	MessageFailureSourceSendError = "send_error"
	// MessageFailureSourceStatusCallback means Meta accepted the message and
	// later reported status=failed via the statuses webhook.
	MessageFailureSourceStatusCallback = "status_callback"
)

// MessageFailedEventData represents data for the message.failed event
type MessageFailedEventData struct {
	MessageID         string             `json:"message_id"`
	WhatsAppMessageID string             `json:"whatsapp_message_id,omitempty"`
	ContactID         string             `json:"contact_id"`
	ContactPhone      string             `json:"contact_phone"`
	ContactName       string             `json:"contact_name"`
	MessageType       models.MessageType `json:"message_type"`
	Content           string             `json:"content"`
	TemplateName      string             `json:"template_name,omitempty"`
	WhatsAppAccount   string             `json:"whatsapp_account"`
	SentByUserID      string             `json:"sent_by_user_id,omitempty"`
	CampaignID        string             `json:"campaign_id,omitempty"`
	Source            string             `json:"source"`
	ErrorCode         int                `json:"error_code,omitempty"` // Meta error code; only set for status_callback failures
	ErrorTitle        string             `json:"error_title,omitempty"`
	ErrorMessage      string             `json:"error_message"`
	ErrorDetails      string             `json:"error_details,omitempty"`
}

// ContactEventData represents data for contact events
type ContactEventData struct {
	ContactID       string `json:"contact_id"`
	ContactPhone    string `json:"contact_phone"`
	ContactName     string `json:"contact_name"`
	WhatsAppAccount string `json:"whatsapp_account"`
}

// TransferEventData represents data for transfer events
type TransferEventData struct {
	TransferID      string                `json:"transfer_id"`
	ContactID       string                `json:"contact_id"`
	ContactPhone    string                `json:"contact_phone"`
	ContactName     string                `json:"contact_name"`
	Source          models.TransferSource `json:"source"`
	Reason          string                `json:"reason,omitempty"`
	AgentID         *string               `json:"agent_id,omitempty"`
	AgentName       *string               `json:"agent_name,omitempty"`
	WhatsAppAccount string                `json:"whatsapp_account"`
}

// maxConcurrentWebhooks limits the number of concurrent webhook deliveries per dispatch
const maxConcurrentWebhooks = 10

// DispatchWebhook sends an event to all matching webhooks for the organization
func (a *App) DispatchWebhook(orgID uuid.UUID, eventType models.WebhookEvent, data any) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		// Use detached context with timeout for webhook delivery
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		a.dispatchWebhookAsync(ctx, orgID, string(eventType), data)
	}()
}

func (a *App) dispatchWebhookAsync(ctx context.Context, orgID uuid.UUID, eventType string, data any) {
	// Find all active webhooks for this org that subscribe to this event (use cache)
	webhooks, err := a.getWebhooksCached(orgID)
	if err != nil {
		a.Log.Error("failed to fetch webhooks", "error", err)
		return
	}

	// Use semaphore to limit concurrent webhook calls
	sem := make(chan struct{}, maxConcurrentWebhooks)
	var wg sync.WaitGroup

	for _, webhook := range webhooks {
		// Check if webhook subscribes to this event
		if !containsEvent(webhook.Events, eventType) {
			continue
		}

		// Check if context was cancelled
		if ctx.Err() != nil {
			a.Log.Warn("webhook dispatch cancelled", "reason", ctx.Err())
			break
		}

		wg.Add(1)
		sem <- struct{}{} // Acquire semaphore slot

		go func(wh models.Webhook) {
			defer wg.Done()
			defer func() { <-sem }() // Release semaphore slot
			a.sendWebhook(ctx, wh, eventType, data)
		}(webhook)
	}

	wg.Wait()
}

func containsEvent(events models.StringArray, event string) bool {
	for _, e := range events {
		if e == event {
			return true
		}
	}
	return false
}

func (a *App) sendWebhook(ctx context.Context, webhook models.Webhook, eventType string, data any) {
	payload := OutboundWebhookPayload{
		Event:     eventType,
		Timestamp: time.Now().UTC(),
		Data:      data,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		a.Log.Error("failed to marshal webhook payload", "error", err, "webhook_id", webhook.ID)
		return
	}

	headers := webhookRequestHeaders(webhook, jsonData)
	deliveryID := a.startWebhookDelivery(webhook, eventType, data, jsonData, headers)

	// Retry logic with exponential backoff
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Check if context was cancelled before retry
		if ctx.Err() != nil {
			a.Log.Warn("webhook delivery cancelled", "reason", ctx.Err(), "webhook_id", webhook.ID)
			a.cancelWebhookDelivery(deliveryID, ctx.Err())
			return
		}

		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s
			select {
			case <-ctx.Done():
				a.Log.Warn("webhook delivery cancelled during backoff", "reason", ctx.Err(), "webhook_id", webhook.ID)
				a.cancelWebhookDelivery(deliveryID, ctx.Err())
				return
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}

		result := a.sendWebhookRequest(ctx, webhook, jsonData, headers)
		a.recordWebhookAttempt(deliveryID, attempt+1, result, result.ok() || attempt == maxRetries-1)
		if !result.ok() {
			a.Log.Warn("webhook delivery failed",
				"error", result.Err,
				"webhook_id", webhook.ID,
				"attempt", attempt+1,
				"max_retries", maxRetries,
			)
			continue
		}

		// Success
		a.Log.Debug("webhook delivered",
			"webhook_id", webhook.ID,
			"event", eventType,
			"url", webhook.URL,
		)
		return
	}

	a.Log.Error("webhook delivery failed after all retries",
		"webhook_id", webhook.ID,
		"event", eventType,
		"url", webhook.URL,
	)
}

// webhookRequestHeaders returns the headers sent with a webhook request: the
// defaults, the webhook's custom headers (which may override the defaults) and
// the HMAC signature. The same map is recorded in the delivery log, so the log
// shows exactly what was sent.
func webhookRequestHeaders(webhook models.Webhook, jsonData []byte) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   "Whatomate-Webhook/1.0",
	}

	// Add custom headers from webhook config
	for key, value := range webhook.Headers {
		if strValue, ok := value.(string); ok {
			headers[http.CanonicalHeaderKey(key)] = strValue
		}
	}

	// Add HMAC signature if secret is configured
	if webhook.Secret != "" {
		headers["X-Webhook-Signature"] = computeHMACSignature(jsonData, webhook.Secret)
	}
	return headers
}

// webhookAttemptResult is the outcome of one webhook HTTP request. Err is nil
// only for a 2xx response.
type webhookAttemptResult struct {
	StatusCode   int // 0 if no response was received
	ResponseBody string
	Duration     time.Duration
	Err          error
}

func (r webhookAttemptResult) ok() bool { return r.Err == nil }

func (a *App) sendWebhookRequest(ctx context.Context, webhook models.Webhook, jsonData []byte, headers map[string]string) webhookAttemptResult {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "POST", webhook.URL, bytes.NewBuffer(jsonData))
	if err != nil {
		return webhookAttemptResult{Err: err, Duration: time.Since(start)}
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// Send request
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return webhookAttemptResult{Err: err, Duration: time.Since(start)}
	}
	defer func() { _ = resp.Body.Close() }()

	// Keep the start of the body for the delivery log, then drain a bounded
	// amount more so the connection can be reused.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, webhookResponseCaptureBytes))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookResponseDrainBytes))

	result := webhookAttemptResult{
		StatusCode:   resp.StatusCode,
		ResponseBody: string(body),
		Duration:     time.Since(start),
	}

	// Check for successful status code (2xx)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Err = &WebhookError{StatusCode: resp.StatusCode}
	}
	return result
}

func computeHMACSignature(data []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(data)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

// WebhookError represents a webhook delivery error
type WebhookError struct {
	StatusCode int
}

func (e *WebhookError) Error() string {
	return "webhook returned non-2xx status: " + http.StatusText(e.StatusCode)
}
