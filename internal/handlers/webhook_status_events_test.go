package handlers

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statusEvents returns the event names of the captured payloads, in order.
func statusEvents(payloads []OutboundWebhookPayload) []string {
	events := make([]string, len(payloads))
	for i, p := range payloads {
		events[i] = p.Event
	}
	return events
}

func unixTimestamp(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestUpdateMessageStatus_DeliveredDispatchesOnce(t *testing.T) {
	app := webhookTestApp(t)
	org, msg, campaign, _ := webhookTestData(t, app, models.MessageStatusSent)
	delivered := captureWebhooks(t, app, org.ID, models.WebhookEventMessageDelivered, models.WebhookEventMessageRead)

	at := time.Date(2026, 10, 27, 10, 15, 2, 0, time.UTC)
	status := WebhookStatus{ID: msg.WhatsAppMessageID, Status: "delivered", Timestamp: unixTimestamp(at), RecipientID: "15550001111"}

	// Meta repeats status callbacks, sometimes at the same moment
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			app.updateMessageStatus(status)
		}()
	}
	wg.Wait()
	app.updateMessageStatus(status)
	app.WaitForBackgroundTasks()

	payloads := delivered()
	require.Len(t, payloads, 1, "a repeated delivered status must fire message.delivered once")
	assert.Equal(t, string(models.WebhookEventMessageDelivered), payloads[0].Event)

	data, ok := payloads[0].Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, msg.ID.String(), data["message_id"])
	assert.Equal(t, msg.WhatsAppMessageID, data["whatsapp_message_id"])
	assert.Equal(t, msg.ContactID.String(), data["contact_id"])
	assert.Equal(t, "15550001111", data["contact_phone"], "Meta's recipient_id wins over the stored contact phone")
	assert.Equal(t, "Test User", data["contact_name"])
	assert.Equal(t, "test message", data["content"])
	assert.Equal(t, campaign.ID.String(), data["campaign_id"])
	assert.Equal(t, "delivered", data["status"])
	assert.Equal(t, at.Format(time.RFC3339), data["status_at"])
	assert.NotEmpty(t, data["delivered_at"])
	assert.NotContains(t, data, "read_at")

	var updated models.Message
	require.NoError(t, app.DB.First(&updated, msg.ID).Error)
	assert.Equal(t, models.MessageStatusDelivered, updated.Status)
	require.NotNil(t, updated.DeliveredAt)
	assert.True(t, updated.DeliveredAt.Equal(at))
}

func TestUpdateMessageStatus_ReadAfterDeliveredDispatchesBoth(t *testing.T) {
	app := webhookTestApp(t)
	org, msg, _, _ := webhookTestData(t, app, models.MessageStatusSent)
	delivered := captureWebhooks(t, app, org.ID, models.WebhookEventMessageDelivered, models.WebhookEventMessageRead)

	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "delivered"})
	app.WaitForBackgroundTasks()
	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "read"})
	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "read"})
	app.WaitForBackgroundTasks()

	payloads := delivered()
	require.Equal(t, []string{"message.delivered", "message.read"}, statusEvents(payloads))
	read, ok := payloads[1].Data.(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, read["delivered_at"], "the read event carries the earlier delivered time")
	assert.NotEmpty(t, read["read_at"])
}

func TestUpdateMessageStatus_ReadBeforeDeliveredDispatchesEachOnce(t *testing.T) {
	app := webhookTestApp(t)
	org, msg, _, _ := webhookTestData(t, app, models.MessageStatusSent)
	delivered := captureWebhooks(t, app, org.ID, models.WebhookEventMessageDelivered, models.WebhookEventMessageRead)

	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "read"})
	app.WaitForBackgroundTasks()
	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "delivered"})
	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "delivered"})
	app.WaitForBackgroundTasks()

	assert.Equal(t, []string{"message.read", "message.delivered"}, statusEvents(delivered()))

	var updated models.Message
	require.NoError(t, app.DB.First(&updated, msg.ID).Error)
	assert.Equal(t, models.MessageStatusRead, updated.Status, "a late delivered status must not move the message back")
	assert.NotNil(t, updated.DeliveredAt)
	assert.NotNil(t, updated.ReadAt)
}

func TestUpdateMessageStatus_FailedMessageDispatchesNoStatusEvents(t *testing.T) {
	app := webhookTestApp(t)
	org, msg, _, _ := webhookTestData(t, app, models.MessageStatusFailed)
	delivered := captureWebhooks(t, app, org.ID, models.WebhookEventMessageDelivered, models.WebhookEventMessageRead)

	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "delivered"})
	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "read"})
	app.WaitForBackgroundTasks()

	assert.Empty(t, delivered())
}

func TestUpdateMessageStatus_OnlySubscribedStatusEventsAreSent(t *testing.T) {
	app := webhookTestApp(t)
	org, msg, _, _ := webhookTestData(t, app, models.MessageStatusSent)
	delivered := captureWebhooks(t, app, org.ID, models.WebhookEventMessageRead)

	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "delivered"})
	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "read"})
	app.WaitForBackgroundTasks()

	assert.Equal(t, []string{"message.read"}, statusEvents(delivered()))
}

func TestUpdateMessageStatus_StatusEventsAppearInWebhookLogs(t *testing.T) {
	app := webhookTestApp(t)
	org, msg, _, _ := webhookTestData(t, app, models.MessageStatusSent)
	captureWebhooks(t, app, org.ID, models.WebhookEventMessageDelivered)

	app.updateMessageStatus(WebhookStatus{ID: msg.WhatsAppMessageID, Status: "delivered", RecipientID: "15550001111"})
	app.WaitForBackgroundTasks()

	var deliveries []models.WebhookDelivery
	require.NoError(t, app.DB.Where("organization_id = ?", org.ID).Find(&deliveries).Error)
	require.Len(t, deliveries, 1)
	assert.Equal(t, "message.delivered", deliveries[0].Event)
	assert.Equal(t, msg.ID.String(), deliveries[0].ReferenceID)
	assert.Equal(t, "15550001111", deliveries[0].ContactPhone)
	assert.Equal(t, models.WebhookDeliverySuccess, deliveries[0].Status)
}

func TestDispatchMessageSent_CampaignMessageCarriesCampaign(t *testing.T) {
	app := webhookTestApp(t)
	org, msg, campaign, _ := webhookTestData(t, app, models.MessageStatusSent)
	delivered := captureWebhooks(t, app, org.ID, models.WebhookEventMessageSent)

	var contact models.Contact
	require.NoError(t, app.DB.First(&contact, msg.ContactID).Error)
	msg.TemplateName = "diwali_offer"
	app.DispatchMessageSent(&models.WhatsAppAccount{OrganizationID: org.ID, Name: msg.WhatsAppAccount}, &contact, &msg)
	app.WaitForBackgroundTasks()

	payloads := delivered()
	require.Len(t, payloads, 1)
	data, ok := payloads[0].Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, msg.ID.String(), data["message_id"])
	assert.Equal(t, campaign.ID.String(), data["campaign_id"])
	assert.Equal(t, "diwali_offer", data["template_name"])
	assert.Equal(t, msg.WhatsAppMessageID, data["whatsapp_message_id"])
}
