package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// metaStatus builds a WebhookStatus the way Meta's callback JSON decodes,
// including the anonymous pricing struct.
func metaStatus(t *testing.T, wamid, status string, unixTS int64, pricingCategory string) WebhookStatus {
	t.Helper()
	payload := map[string]any{"id": wamid, "status": status, "timestamp": jsonInt(unixTS)}
	if pricingCategory != "" {
		payload["pricing"] = map[string]any{"billable": true, "pricing_model": "PMP", "category": pricingCategory}
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	var s WebhookStatus
	require.NoError(t, json.Unmarshal(raw, &s))
	return s
}

// jsonInt renders a unix timestamp as Meta does: a decimal string.
func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func reloadMessage(t *testing.T, app *App, id uuid.UUID) models.Message {
	t.Helper()
	var m models.Message
	require.NoError(t, app.DB.First(&m, id).Error)
	return m
}

func TestUpdateMessageStatus_RecordsMetaTimestamps(t *testing.T) {
	app := webhookTestApp(t)
	_, msg, _, _ := webhookTestData(t, app, models.MessageStatusSent)
	const sentTS, deliveredTS, readTS = 1_760_000_000, 1_760_000_002, 1_760_000_160

	app.updateMessageStatus(metaStatus(t, msg.WhatsAppMessageID, "delivered", deliveredTS, ""))
	app.updateMessageStatus(metaStatus(t, msg.WhatsAppMessageID, "read", readTS, ""))
	// Meta's "sent" can arrive last; it fills sent_at without moving the status back.
	app.updateMessageStatus(metaStatus(t, msg.WhatsAppMessageID, "sent", sentTS, ""))

	got := reloadMessage(t, app, msg.ID)
	assert.Equal(t, models.MessageStatusRead, got.Status)
	require.NotNil(t, got.SentAt)
	require.NotNil(t, got.DeliveredAt)
	require.NotNil(t, got.ReadAt)
	assert.Equal(t, int64(sentTS), got.SentAt.Unix())
	assert.Equal(t, int64(deliveredTS), got.DeliveredAt.Unix())
	assert.Equal(t, int64(readTS), got.ReadAt.Unix())
}

func TestUpdateMessageStatus_LateDeliveredAfterReadRecordsTimestamp(t *testing.T) {
	app := webhookTestApp(t)
	_, msg, _, _ := webhookTestData(t, app, models.MessageStatusSent)

	app.updateMessageStatus(metaStatus(t, msg.WhatsAppMessageID, "read", 1_760_000_100, ""))
	got := reloadMessage(t, app, msg.ID)
	assert.Equal(t, models.MessageStatusRead, got.Status)
	assert.Nil(t, got.DeliveredAt, "timestamps are never invented")

	app.updateMessageStatus(metaStatus(t, msg.WhatsAppMessageID, "delivered", 1_760_000_050, ""))
	got = reloadMessage(t, app, msg.ID)
	assert.Equal(t, models.MessageStatusRead, got.Status, "status does not move backwards")
	require.NotNil(t, got.DeliveredAt)
	assert.Equal(t, int64(1_760_000_050), got.DeliveredAt.Unix())
}

func TestUpdateMessageStatus_TimestampsAndPricingSetOnce(t *testing.T) {
	app := webhookTestApp(t)
	_, msg, _, _ := webhookTestData(t, app, models.MessageStatusSent)

	app.updateMessageStatus(metaStatus(t, msg.WhatsAppMessageID, "delivered", 1_760_000_010, "utility"))
	app.updateMessageStatus(metaStatus(t, msg.WhatsAppMessageID, "delivered", 1_760_000_999, "marketing"))

	got := reloadMessage(t, app, msg.ID)
	require.NotNil(t, got.DeliveredAt)
	assert.Equal(t, int64(1_760_000_010), got.DeliveredAt.Unix())
	assert.Equal(t, "utility", got.PricingCategory)
}

func TestFinalizeMessageSend_SuccessSetsSentAt(t *testing.T) {
	app := webhookTestApp(t)
	org, msg, _, _ := webhookTestData(t, app, models.MessageStatusPending)
	var contact models.Contact
	require.NoError(t, app.DB.First(&contact, msg.ContactID).Error)
	account := models.WhatsAppAccount{OrganizationID: org.ID, Name: msg.WhatsAppAccount}

	before := time.Now().Add(-time.Second)
	app.finalizeMessageSend(&msg, OutgoingMessageRequest{Account: &account, Contact: &contact}, SLASendOptions(), "wamid.accepted", nil)

	got := reloadMessage(t, app, msg.ID)
	assert.Equal(t, models.MessageStatusSent, got.Status)
	require.NotNil(t, got.SentAt)
	assert.True(t, got.SentAt.After(before))
}

func TestUserSendOptions_RecordsAPISourceForAPIKeyRequests(t *testing.T) {
	userID := uuid.New()

	sessionReq := testutil.NewGETRequest(t)
	opts := userSendOptions(sessionReq, userID)
	assert.Equal(t, models.MessageSourceAgent, opts.Source)
	require.NotNil(t, opts.SentByUserID)
	assert.Equal(t, userID, *opts.SentByUserID)

	apiReq := testutil.NewGETRequest(t)
	apiReq.RequestCtx.SetUserValue(middleware.ContextKeyAPIKeyAuth, true)
	assert.Equal(t, models.MessageSourceAPI, userSendOptions(apiReq, userID).Source)
}

func TestSendOptionPresets_Sources(t *testing.T) {
	assert.Equal(t, models.MessageSourceAgent, DefaultSendOptions().Source)
	assert.Equal(t, models.MessageSourceChatbot, ChatbotSendOptions().Source)
	assert.Equal(t, models.MessageSourceAPI, APISendOptions().Source)
	assert.Equal(t, models.MessageSourceAutomation, SLASendOptions().Source)
}

func TestSaveIncomingMessage_RecordsIncomingSource(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	wamid := "wamid.in_" + uuid.New().String()[:8]
	app.saveIncomingMessage(account, contact, wamid, "text", "hi", nil, "")
	app.WaitForBackgroundTasks()

	var m models.Message
	require.NoError(t, app.DB.Where("whats_app_message_id = ?", wamid).First(&m).Error)
	assert.Equal(t, models.MessageSourceIncoming, m.Source)
}

func TestMessageLogCursor_RoundTrip(t *testing.T) {
	row := messageLogRow{ID: uuid.New(), CreatedAt: time.Date(2026, 10, 24, 10, 49, 53, 123456000, time.UTC)}
	c, err := decodeLogCursor(encodeLogCursor(row.CreatedAt, row.ID))
	require.NoError(t, err)
	assert.Equal(t, row.ID, c.ID)
	assert.True(t, row.CreatedAt.Equal(c.CreatedAt))

	_, err = decodeLogCursor("bm90LWEtY3Vyc29y") // base64 of "not-a-cursor"
	assert.Error(t, err)
}

func TestNormalizePhoneFilter(t *testing.T) {
	assert.Equal(t, "919876543210", normalizePhoneFilter("+91 98765-43210"))
	assert.Equal(t, "", normalizePhoneFilter("abc"))
}

func TestWithStatementTimeout_CancelsSlowQueriesWith422(t *testing.T) {
	app := webhookTestApp(t)

	err := app.withStatementTimeout(func(tx *gorm.DB) error {
		return tx.Exec("SELECT pg_sleep(10)").Error
	})
	require.Error(t, err)

	req := testutil.NewGETRequest(t)
	require.NoError(t, app.sendLogQueryError(req, err, "message logs"))
	assert.Equal(t, fasthttp.StatusUnprocessableEntity, testutil.GetResponseStatusCode(req))
}
