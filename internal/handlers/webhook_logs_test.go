package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// webhookLogsFixture is an org with an admin, a webhook pointing at a local
// test server, and the server's received request bodies.
type webhookLogsFixture struct {
	app     *App
	org     models.Organization
	user    *models.User
	webhook models.Webhook

	mu       sync.Mutex
	received [][]byte
	// respond decides each response; call is 1-based.
	respond func(call int32, w http.ResponseWriter)
	calls   atomic.Int32
}

func newWebhookLogsFixture(t *testing.T) *webhookLogsFixture {
	t.Helper()
	app := webhookTestApp(t)
	app.HTTPClient = &http.Client{Timeout: 5 * time.Second}

	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))

	f := &webhookLogsFixture{app: app, org: *org, user: user}
	f.respond = func(int32, http.ResponseWriter) {}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.received = append(f.received, body)
		respond := f.respond
		f.mu.Unlock()
		respond(f.calls.Add(1), w)
	}))
	t.Cleanup(server.Close)

	f.webhook = models.Webhook{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "SDR Bot Delivery",
		URL:            server.URL,
		Events:         models.StringArray{string(models.WebhookEventMessageSent)},
		Headers:        models.JSONB{"X-Custom": "value"},
		Secret:         "s3cret",
		IsActive:       true,
	}
	require.NoError(t, app.DB.Create(&f.webhook).Error)
	return f
}

// setRespond replaces the test server's response behaviour. The server runs
// on its own goroutines, hence the lock.
func (f *webhookLogsFixture) setRespond(fn func(call int32, w http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = fn
}

func (f *webhookLogsFixture) receivedBodies() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.received...)
}

func (f *webhookLogsFixture) onlyDelivery(t *testing.T) models.WebhookDelivery {
	t.Helper()
	var deliveries []models.WebhookDelivery
	require.NoError(t, f.app.DB.Where("webhook_id = ?", f.webhook.ID).Find(&deliveries).Error)
	require.Len(t, deliveries, 1)
	return deliveries[0]
}

var sentEvent = MessageEventData{MessageID: "msg-123", ContactPhone: "919876543210", MessageType: models.MessageTypeText, Content: "hi"}

func TestSendWebhook_RecordsSuccessfulDelivery(t *testing.T) {
	f := newWebhookLogsFixture(t)
	f.setRespond(func(_ int32, w http.ResponseWriter) { _, _ = w.Write([]byte(`{"ok":true}`)) })

	f.app.sendWebhook(context.Background(), f.webhook, string(models.WebhookEventMessageSent), sentEvent)

	d := f.onlyDelivery(t)
	assert.Equal(t, models.WebhookDeliverySuccess, d.Status)
	assert.Equal(t, 1, d.AttemptCount)
	assert.Equal(t, http.StatusOK, d.ResponseStatus)
	assert.Equal(t, `{"ok":true}`, d.ResponseBody)
	assert.Empty(t, d.FailureReason)
	assert.NotNil(t, d.CompletedAt)
	assert.Equal(t, "SDR Bot Delivery", d.WebhookName)
	assert.Equal(t, "message.sent", d.Event)
	assert.Equal(t, "msg-123", d.ReferenceID)
	assert.Equal(t, "919876543210", d.ContactPhone)
	assert.Len(t, d.Attempts, 1)

	// The snapshot is exactly what the receiver got, including headers as sent.
	received := f.receivedBodies()
	require.Len(t, received, 1)
	assert.Equal(t, string(received[0]), d.RequestBody)
	assert.Equal(t, "value", d.RequestHeaders["X-Custom"])
	assert.Equal(t, "application/json", d.RequestHeaders["Content-Type"])
	assert.Equal(t, computeHMACSignature(received[0], "s3cret"), d.RequestHeaders["X-Webhook-Signature"])
	for _, v := range d.RequestHeaders {
		assert.NotContains(t, v, "s3cret", "the signing secret itself is never recorded")
	}
}

func TestSendWebhook_RecordsEveryAttemptThenFailure(t *testing.T) {
	f := newWebhookLogsFixture(t)
	f.setRespond(func(_ int32, w http.ResponseWriter) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("down for maintenance"))
	})

	f.app.sendWebhook(context.Background(), f.webhook, string(models.WebhookEventMessageSent), sentEvent)

	d := f.onlyDelivery(t)
	assert.Equal(t, models.WebhookDeliveryFailed, d.Status)
	assert.Equal(t, 3, d.AttemptCount)
	assert.Equal(t, "HTTP 503 Service Unavailable", d.FailureReason)
	assert.Equal(t, "down for maintenance", d.ResponseBody)
	assert.NotNil(t, d.CompletedAt)
	require.Len(t, d.Attempts, 3)
	for i, a := range d.Attempts {
		entry := a.(map[string]any)
		assert.EqualValues(t, i+1, entry["attempt"])
		assert.EqualValues(t, 503, entry["status_code"])
	}
}

func TestSendWebhook_SuccessAfterRetry(t *testing.T) {
	f := newWebhookLogsFixture(t)
	f.setRespond(func(call int32, w http.ResponseWriter) {
		if call == 1 {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	f.app.sendWebhook(context.Background(), f.webhook, string(models.WebhookEventMessageSent), sentEvent)

	d := f.onlyDelivery(t)
	assert.Equal(t, models.WebhookDeliverySuccess, d.Status)
	assert.Equal(t, 2, d.AttemptCount)
	assert.Empty(t, d.FailureReason, "a later success clears the earlier failure")
	require.Len(t, d.Attempts, 2)
	assert.Equal(t, "HTTP 500 Internal Server Error", d.Attempts[0].(map[string]any)["error"])
}

func TestSendWebhook_CancelledDeliveryIsClosedOut(t *testing.T) {
	f := newWebhookLogsFixture(t)
	f.setRespond(func(_ int32, w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) })

	// The context ends during the 2s backoff before the second attempt.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	f.app.sendWebhook(ctx, f.webhook, string(models.WebhookEventMessageSent), sentEvent)

	d := f.onlyDelivery(t)
	assert.Equal(t, models.WebhookDeliveryFailed, d.Status)
	assert.Equal(t, 1, d.AttemptCount)
	assert.Contains(t, d.FailureReason, "cancelled")
	assert.NotNil(t, d.CompletedAt)
}

func TestSendWebhook_CapsRecordedResponseBody(t *testing.T) {
	f := newWebhookLogsFixture(t)
	f.setRespond(func(_ int32, w http.ResponseWriter) { _, _ = w.Write([]byte(strings.Repeat("a", 10_000))) })

	f.app.sendWebhook(context.Background(), f.webhook, string(models.WebhookEventMessageSent), sentEvent)

	assert.Len(t, f.onlyDelivery(t).ResponseBody, webhookResponseCaptureBytes)
}

func TestTestWebhook_RecordsDeliveryWithReadableReason(t *testing.T) {
	f := newWebhookLogsFixture(t)
	// Point the webhook at a server that's gone: connection refused.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	require.NoError(t, f.app.DB.Model(&f.webhook).Update("url", dead.URL).Error)

	req := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(req, f.org.ID, f.user.ID)
	testutil.SetPathParam(req, "id", f.webhook.ID.String())
	require.NoError(t, f.app.TestWebhook(req))
	assert.Equal(t, fasthttp.StatusBadGateway, testutil.GetResponseStatusCode(req))
	assert.Contains(t, string(testutil.GetResponseBody(req)), "connection refused")

	d := f.onlyDelivery(t)
	assert.Equal(t, "test", d.Event)
	assert.Equal(t, models.WebhookDeliveryFailed, d.Status)
	assert.Equal(t, 0, d.ResponseStatus)
	assert.Contains(t, d.FailureReason, "connection refused")
}

func TestSanitizeLogText(t *testing.T) {
	assert.Equal(t, "ab", sanitizeLogText("a\x00b"))
	assert.Equal(t, "a�b", sanitizeLogText("a\xffb"))
}

func TestFailStaleWebhookDeliveries(t *testing.T) {
	f := newWebhookLogsFixture(t)
	stale := f.insertDelivery(t, 20*time.Minute, models.WebhookDeliveryProcessing, nil)
	inFlight := f.insertDelivery(t, time.Minute, models.WebhookDeliveryProcessing, nil)
	done := f.insertDelivery(t, time.Hour, models.WebhookDeliverySuccess, nil)

	assert.GreaterOrEqual(t, f.app.FailStaleWebhookDeliveries(), int64(1))

	reload := func(id uuid.UUID) models.WebhookDelivery {
		var d models.WebhookDelivery
		require.NoError(t, f.app.DB.First(&d, "id = ?", id).Error)
		return d
	}
	assert.Equal(t, models.WebhookDeliveryFailed, reload(stale.ID).Status)
	assert.Contains(t, reload(stale.ID).FailureReason, "Interrupted")
	assert.Equal(t, models.WebhookDeliveryProcessing, reload(inFlight.ID).Status, "recent deliveries may still be retrying")
	assert.Equal(t, models.WebhookDeliverySuccess, reload(done.ID).Status)
}

// insertDelivery stores a delivery created `age` ago; mutate adjusts it first.
func (f *webhookLogsFixture) insertDelivery(t *testing.T, age time.Duration, status models.WebhookDeliveryStatus, mutate func(*models.WebhookDelivery)) models.WebhookDelivery {
	t.Helper()
	d := models.WebhookDelivery{
		ID:             uuid.New(),
		CreatedAt:      time.Now().Add(-age),
		OrganizationID: f.org.ID,
		WebhookID:      f.webhook.ID,
		WebhookName:    f.webhook.Name,
		Event:          "message.sent",
		ReferenceID:    "msg-" + uuid.New().String()[:8],
		ContactPhone:   "919800000001",
		Status:         status,
		RequestURL:     f.webhook.URL,
		RequestHeaders: models.JSONB{"Content-Type": "application/json"},
		RequestBody:    `{"event":"message.sent"}`,
		Attempts:       models.JSONBArray{},
	}
	if mutate != nil {
		mutate(&d)
	}
	require.NoError(t, f.app.DB.Create(&d).Error)
	return d
}

func (f *webhookLogsFixture) listLogs(t *testing.T, userID uuid.UUID, params map[string]string) (int, []WebhookLogResponse) {
	t.Helper()
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, f.org.ID, userID)
	testutil.SetQueryParam(req, "from", time.Now().AddDate(0, 0, -30).Format(time.DateOnly))
	testutil.SetQueryParam(req, "to", time.Now().AddDate(0, 0, 1).Format(time.DateOnly))
	for k, v := range params {
		testutil.SetQueryParam(req, k, v)
	}
	require.NoError(t, f.app.ListWebhookLogs(req))
	var resp struct {
		Data struct {
			Logs []WebhookLogResponse `json:"logs"`
		} `json:"data"`
	}
	status := testutil.GetResponseStatusCode(req)
	if status == fasthttp.StatusOK {
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	}
	return status, resp.Data.Logs
}

func deliveryIDs(logs []WebhookLogResponse) []uuid.UUID {
	ids := make([]uuid.UUID, len(logs))
	for i, l := range logs {
		ids[i] = l.ID
	}
	return ids
}

func TestListWebhookLogs_FiltersAndSearch(t *testing.T) {
	f := newWebhookLogsFixture(t)
	ok := f.insertDelivery(t, time.Hour, models.WebhookDeliverySuccess, nil)
	failed := f.insertDelivery(t, 2*time.Hour, models.WebhookDeliveryFailed, func(d *models.WebhookDelivery) {
		d.FailureReason = "HTTP 503 Service Unavailable"
		d.ContactPhone = "919811112222"
	})
	otherHook := f.insertDelivery(t, 3*time.Hour, models.WebhookDeliverySuccess, func(d *models.WebhookDelivery) {
		d.WebhookID = uuid.New()
		d.Event = "contact.created"
	})

	// Another org's delivery never shows.
	other := newWebhookLogsFixture(t)
	other.insertDelivery(t, time.Hour, models.WebhookDeliverySuccess, nil)

	tests := []struct {
		name   string
		params map[string]string
		want   []uuid.UUID
	}{
		{"all, newest first", nil, []uuid.UUID{ok.ID, failed.ID, otherHook.ID}},
		{"status", map[string]string{"status": "failed"}, []uuid.UUID{failed.ID}},
		{"webhook", map[string]string{"webhook_id": f.webhook.ID.String()}, []uuid.UUID{ok.ID, failed.ID}},
		{"event", map[string]string{"event": "contact.created"}, []uuid.UUID{otherHook.ID}},
		{"search reference", map[string]string{"search": ok.ReferenceID}, []uuid.UUID{ok.ID}},
		{"search phone any format", map[string]string{"search": "+91 98111 12222"}, []uuid.UUID{failed.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, logs := f.listLogs(t, f.user.ID, tt.params)
			require.Equal(t, fasthttp.StatusOK, status)
			assert.Equal(t, tt.want, deliveryIDs(logs))
		})
	}

	t.Run("invalid status", func(t *testing.T) {
		status, _ := f.listLogs(t, f.user.ID, map[string]string{"status": "sent"})
		assert.Equal(t, fasthttp.StatusBadRequest, status)
	})
	t.Run("requires webhook_logs:read", func(t *testing.T) {
		agentRole := testutil.CreateAgentRole(t, f.app.DB, f.org.ID)
		agent := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&agentRole.ID))
		status, _ := f.listLogs(t, agent.ID, nil)
		assert.Equal(t, fasthttp.StatusForbidden, status)
	})
}

func TestGetWebhookLog_ReturnsSnapshot(t *testing.T) {
	f := newWebhookLogsFixture(t)
	f.setRespond(func(_ int32, w http.ResponseWriter) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("queued"))
	})
	f.app.sendWebhook(context.Background(), f.webhook, string(models.WebhookEventMessageSent), sentEvent)
	d := f.onlyDelivery(t)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, f.org.ID, f.user.ID)
	testutil.SetPathParam(req, "id", d.ID.String())
	require.NoError(t, f.app.GetWebhookLog(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data struct {
			Request struct {
				Method  string            `json:"method"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
				Body    map[string]any    `json:"body"`
			} `json:"request"`
			Response struct {
				StatusCode int    `json:"status_code"`
				Body       string `json:"body"`
			} `json:"response"`
			Attempts []map[string]any `json:"attempts"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.Equal(t, "POST", resp.Data.Request.Method)
	assert.Equal(t, f.webhook.URL, resp.Data.Request.URL)
	assert.Equal(t, "value", resp.Data.Request.Headers["X-Custom"])
	assert.Equal(t, "message.sent", resp.Data.Request.Body["event"], "body is returned as JSON, not a string")
	assert.Equal(t, http.StatusAccepted, resp.Data.Response.StatusCode)
	assert.Equal(t, "queued", resp.Data.Response.Body)
	assert.Len(t, resp.Data.Attempts, 1)

	// Another org can't read it.
	other := newWebhookLogsFixture(t)
	req = testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, other.org.ID, other.user.ID)
	testutil.SetPathParam(req, "id", d.ID.String())
	require.NoError(t, other.app.GetWebhookLog(req))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func (f *webhookLogsFixture) purge(t *testing.T, userID uuid.UUID, body PurgeWebhookLogsRequest) (int, map[string]any) {
	t.Helper()
	req := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(req, f.org.ID, userID)
	require.NoError(t, f.app.PurgeWebhookLogs(req))
	var resp struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	return testutil.GetResponseStatusCode(req), resp.Data
}

func TestPurgeWebhookLogs(t *testing.T) {
	f := newWebhookLogsFixture(t)
	day := 24 * time.Hour
	old1 := f.insertDelivery(t, 20*day, models.WebhookDeliverySuccess, nil)
	old2 := f.insertDelivery(t, 15*day, models.WebhookDeliveryFailed, nil)
	oldOtherHook := f.insertDelivery(t, 20*day, models.WebhookDeliverySuccess, func(d *models.WebhookDelivery) { d.WebhookID = uuid.New() })
	recent := f.insertDelivery(t, 3*day, models.WebhookDeliverySuccess, nil)
	cutoff := time.Now().Add(-10 * day).Format(time.DateOnly)

	t.Run("refuses cutoffs within the last 7 days", func(t *testing.T) {
		status, _ := f.purge(t, f.user.ID, PurgeWebhookLogsRequest{Before: time.Now().Add(-3 * day).Format(time.DateOnly), Confirm: true})
		assert.Equal(t, fasthttp.StatusBadRequest, status)
	})

	t.Run("preview counts without deleting", func(t *testing.T) {
		status, data := f.purge(t, f.user.ID, PurgeWebhookLogsRequest{Before: cutoff})
		require.Equal(t, fasthttp.StatusOK, status)
		assert.EqualValues(t, 3, data["matching"])
		assert.Equal(t, false, data["started"])
	})

	t.Run("blocked while another purge runs", func(t *testing.T) {
		lockKey := "webhook_logs:purge:" + f.org.ID.String()
		require.NoError(t, f.app.Redis.Set(context.Background(), lockKey, "someone", time.Minute).Err())
		defer f.app.Redis.Del(context.Background(), lockKey)
		status, _ := f.purge(t, f.user.ID, PurgeWebhookLogsRequest{Before: cutoff, Confirm: true})
		assert.Equal(t, fasthttp.StatusConflict, status)
	})

	t.Run("admin only", func(t *testing.T) {
		managerRole := testutil.CreateTestRoleWithKeys(t, f.app.DB, f.org.ID, "manager", []string{"webhook_logs:read"})
		manager := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&managerRole.ID))
		status, _ := f.purge(t, manager.ID, PurgeWebhookLogsRequest{Before: cutoff, Confirm: true})
		assert.Equal(t, fasthttp.StatusForbidden, status)
	})

	t.Run("confirmed purge of one webhook deletes only its old deliveries", func(t *testing.T) {
		status, data := f.purge(t, f.user.ID, PurgeWebhookLogsRequest{Before: cutoff, WebhookID: f.webhook.ID.String(), Confirm: true})
		require.Equal(t, fasthttp.StatusOK, status)
		assert.EqualValues(t, 2, data["matching"])
		assert.Equal(t, true, data["started"])
		f.app.WaitForBackgroundTasks()

		var remaining []uuid.UUID
		require.NoError(t, f.app.DB.Model(&models.WebhookDelivery{}).Where("organization_id = ?", f.org.ID).Pluck("id", &remaining).Error)
		assert.ElementsMatch(t, []uuid.UUID{oldOtherHook.ID, recent.ID}, remaining)
		assert.NotContains(t, remaining, old1.ID)
		assert.NotContains(t, remaining, old2.ID)

		var audits int64
		f.app.DB.Model(&models.AuditLog{}).Where("organization_id = ? AND resource_type = ?", f.org.ID, models.ResourceWebhookLogs).Count(&audits)
		assert.EqualValues(t, 1, audits, "the purge is recorded in the audit log")

		lockKey := "webhook_logs:purge:" + f.org.ID.String()
		assert.Zero(t, f.app.Redis.Exists(context.Background(), lockKey).Val(), "the lock is released afterwards")
	})
}
