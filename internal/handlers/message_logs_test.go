package handlers_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

type messageLogsPage struct {
	Logs       []handlers.MessageLogResponse `json:"logs"`
	NextCursor string                        `json:"next_cursor"`
	PrevCursor string                        `json:"prev_cursor"`
}

// messageLogsFixture is an org with an admin user, a WhatsApp account and a
// contact to attach messages to.
type messageLogsFixture struct {
	app     *handlers.App
	org     *models.Organization
	user    *models.User
	account *models.WhatsAppAccount
	contact *models.Contact
}

func newMessageLogsFixture(t *testing.T) *messageLogsFixture {
	t.Helper()
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID,
		testutil.WithContactAccount(account.Name),
		testutil.WithPhoneNumber(uniqueTestPhone("9198")))
	return &messageLogsFixture{app: app, org: org, user: user, account: account, contact: contact}
}

// createLogMessage inserts a message `age` ago; mutate adjusts fields first.
func (f *messageLogsFixture) createLogMessage(t *testing.T, age time.Duration, mutate func(m *models.Message)) models.Message {
	t.Helper()
	m := models.Message{
		BaseModel:       models.BaseModel{ID: uuid.New(), CreatedAt: time.Now().Add(-age)},
		OrganizationID:  f.org.ID,
		WhatsAppAccount: f.account.Name,
		ContactID:       f.contact.ID,
		Direction:       models.DirectionOutgoing,
		MessageType:     models.MessageTypeText,
		Content:         "hello",
		Status:          models.MessageStatusSent,
		Source:          models.MessageSourceAPI,
	}
	if mutate != nil {
		mutate(&m)
	}
	require.NoError(t, f.app.DB.Create(&m).Error)
	return m
}

// listLogs calls ListMessageLogs with the last 7 days plus params and returns
// the status code and decoded page.
func (f *messageLogsFixture) listLogs(t *testing.T, params map[string]string) (int, messageLogsPage) {
	t.Helper()
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, f.org.ID, f.user.ID)
	testutil.SetQueryParam(req, "from", time.Now().AddDate(0, 0, -7).Format(time.DateOnly))
	testutil.SetQueryParam(req, "to", time.Now().AddDate(0, 0, 1).Format(time.DateOnly))
	for k, v := range params {
		testutil.SetQueryParam(req, k, v)
	}
	require.NoError(t, f.app.ListMessageLogs(req))

	var resp struct {
		Data messageLogsPage `json:"data"`
	}
	status := testutil.GetResponseStatusCode(req)
	if status == fasthttp.StatusOK {
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	}
	return status, resp.Data
}

// uniqueTestPhone returns a digits-only phone number unique to this test run.
func uniqueTestPhone(prefix string) string {
	return fmt.Sprintf("%s%08d", prefix, rand.Intn(100_000_000))
}

func logIDs(logs []handlers.MessageLogResponse) []uuid.UUID {
	ids := make([]uuid.UUID, len(logs))
	for i, l := range logs {
		ids[i] = l.ID
	}
	return ids
}

func TestListMessageLogs_ReturnsOrgMessagesNewestFirst(t *testing.T) {
	f := newMessageLogsFixture(t)
	older := f.createLogMessage(t, 2*time.Hour, nil)
	newer := f.createLogMessage(t, time.Hour, func(m *models.Message) {
		m.MessageType = models.MessageTypeTemplate
		m.TemplateName = "booking_confirmation"
	})

	// Another org's message must never appear.
	other := newMessageLogsFixture(t)
	other.createLogMessage(t, time.Hour, nil)

	status, page := f.listLogs(t, nil)
	require.Equal(t, fasthttp.StatusOK, status)
	require.Equal(t, []uuid.UUID{newer.ID, older.ID}, logIDs(page.Logs))

	got := page.Logs[0]
	assert.Equal(t, f.contact.PhoneNumber, got.ContactPhone)
	assert.Equal(t, f.contact.ProfileName, got.ContactName)
	assert.Equal(t, "booking_confirmation", got.TemplateName)
	assert.Equal(t, models.MessageSourceAPI, got.Source)
	assert.Empty(t, page.NextCursor)
	assert.Empty(t, page.PrevCursor)
}

func TestListMessageLogs_Filters(t *testing.T) {
	f := newMessageLogsFixture(t)
	failed := f.createLogMessage(t, time.Hour, func(m *models.Message) {
		m.Status = models.MessageStatusFailed
		m.ErrorMessage = "131026: Message undeliverable"
	})
	incoming := f.createLogMessage(t, 2*time.Hour, func(m *models.Message) {
		m.Direction = models.DirectionIncoming
		m.Status = models.MessageStatusReceived
		m.Source = models.MessageSourceIncoming
	})
	tmpl := f.createLogMessage(t, 3*time.Hour, func(m *models.Message) {
		m.MessageType = models.MessageTypeTemplate
		m.TemplateName = "otp_v1"
		m.Source = models.MessageSourceCampaign
	})
	otherAccount := f.createLogMessage(t, 4*time.Hour, func(m *models.Message) {
		m.WhatsAppAccount = "second-account"
	})

	other := testutil.CreateTestContactWith(t, f.app.DB, f.org.ID, testutil.WithPhoneNumber(uniqueTestPhone("9177")))
	otherContact := f.createLogMessage(t, 5*time.Hour, func(m *models.Message) { m.ContactID = other.ID })

	tests := []struct {
		name   string
		params map[string]string
		want   []uuid.UUID
	}{
		{"status", map[string]string{"status": "failed"}, []uuid.UUID{failed.ID}},
		{"direction", map[string]string{"direction": "incoming"}, []uuid.UUID{incoming.ID}},
		{"source", map[string]string{"source": "campaign"}, []uuid.UUID{tmpl.ID}},
		{"template", map[string]string{"template": "otp_v1"}, []uuid.UUID{tmpl.ID}},
		{"account", map[string]string{"account": "second-account"}, []uuid.UUID{otherAccount.ID}},
		{"phone with formatting", map[string]string{"phone": "+" + other.PhoneNumber[:2] + " " + other.PhoneNumber[2:]}, []uuid.UUID{otherContact.ID}},
		{"unknown phone", map[string]string{"phone": "10000000000"}, []uuid.UUID{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, page := f.listLogs(t, tt.params)
			require.Equal(t, fasthttp.StatusOK, status)
			assert.Equal(t, tt.want, logIDs(page.Logs))
		})
	}
}

func TestListMessageLogs_Validation(t *testing.T) {
	f := newMessageLogsFixture(t)

	tests := []struct {
		name   string
		params map[string]string
	}{
		{"older than 90 days", map[string]string{"from": time.Now().AddDate(0, 0, -100).Format(time.DateOnly)}},
		{"reversed range", map[string]string{"from": time.Now().Format(time.DateOnly), "to": time.Now().AddDate(0, 0, -2).Format(time.DateOnly)}},
		{"bad date", map[string]string{"from": "yesterday"}},
		{"bad status", map[string]string{"status": "bounced"}},
		{"bad cursor", map[string]string{"cursor": "not-a-cursor"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _ := f.listLogs(t, tt.params)
			assert.Equal(t, fasthttp.StatusBadRequest, status)
		})
	}

	t.Run("missing range", func(t *testing.T) {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, f.org.ID, f.user.ID)
		require.NoError(t, f.app.ListMessageLogs(req))
		assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
	})
}

func TestListMessageLogs_CursorPagination(t *testing.T) {
	f := newMessageLogsFixture(t)
	var all []uuid.UUID // newest first
	for i := 0; i < 120; i++ {
		all = append(all, f.createLogMessage(t, time.Duration(i+1)*time.Minute, nil).ID)
	}

	status, page1 := f.listLogs(t, nil)
	require.Equal(t, fasthttp.StatusOK, status)
	require.Len(t, page1.Logs, 50)
	assert.Empty(t, page1.PrevCursor, "first page has nothing newer")
	require.NotEmpty(t, page1.NextCursor)

	_, page2 := f.listLogs(t, map[string]string{"cursor": page1.NextCursor, "dir": "older"})
	require.Len(t, page2.Logs, 50)
	require.NotEmpty(t, page2.PrevCursor)

	_, page3 := f.listLogs(t, map[string]string{"cursor": page2.NextCursor, "dir": "older"})
	require.Len(t, page3.Logs, 20)
	assert.Empty(t, page3.NextCursor, "last page has nothing older")

	walked := append(append(logIDs(page1.Logs), logIDs(page2.Logs)...), logIDs(page3.Logs)...)
	assert.Equal(t, all, walked, "older pages cover every row exactly once, in order")

	// Going newer from page 3 returns page 2 again, newest first.
	_, back := f.listLogs(t, map[string]string{"cursor": page3.PrevCursor, "dir": "newer"})
	assert.Equal(t, logIDs(page2.Logs), logIDs(back.Logs))
	assert.NotEmpty(t, back.PrevCursor)
	assert.NotEmpty(t, back.NextCursor)
}

func TestListMessageLogs_MasksPhoneNumbersWhenEnabled(t *testing.T) {
	f := newMessageLogsFixture(t)
	require.NoError(t, f.app.DB.Model(f.org).Update("settings", models.JSONB{"mask_phone_numbers": true}).Error)
	f.createLogMessage(t, time.Hour, nil)

	_, page := f.listLogs(t, nil)
	require.Len(t, page.Logs, 1)
	phone := page.Logs[0].ContactPhone
	assert.NotEqual(t, f.contact.PhoneNumber, phone)
	assert.True(t, strings.HasSuffix(phone, f.contact.PhoneNumber[len(f.contact.PhoneNumber)-4:]))
	assert.Contains(t, phone, "*")
}

func TestListMessageLogs_KeepsDeletedContactDetails(t *testing.T) {
	f := newMessageLogsFixture(t)
	f.createLogMessage(t, time.Hour, nil)
	require.NoError(t, f.app.DB.Delete(f.contact).Error)

	_, page := f.listLogs(t, nil)
	require.Len(t, page.Logs, 1)
	assert.Equal(t, f.contact.PhoneNumber, page.Logs[0].ContactPhone)

	// The phone filter still finds a deleted contact's messages.
	_, byPhone := f.listLogs(t, map[string]string{"phone": f.contact.PhoneNumber})
	assert.Len(t, byPhone.Logs, 1)
}

func TestListMessageLogs_TruncatesContentAndResolvesCampaign(t *testing.T) {
	f := newMessageLogsFixture(t)
	tmpl := testutil.CreateTestTemplate(t, f.app.DB, f.org.ID, f.account.Name)
	campaign := models.BulkMessageCampaign{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  f.org.ID,
		WhatsAppAccount: f.account.Name,
		Name:            "October promo",
		TemplateID:      tmpl.ID,
		CreatedBy:       f.user.ID,
	}
	require.NoError(t, f.app.DB.Create(&campaign).Error)

	long := strings.Repeat("a", 500)
	m := f.createLogMessage(t, time.Hour, func(m *models.Message) {
		m.Content = long
		m.Metadata = models.JSONB{"campaign_id": campaign.ID.String()}
	})

	_, page := f.listLogs(t, nil)
	require.Len(t, page.Logs, 1)
	assert.Len(t, page.Logs[0].Content, 200)
	assert.Equal(t, "October promo", page.Logs[0].CampaignName)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, f.org.ID, f.user.ID)
	testutil.SetPathParam(req, "id", m.ID.String())
	require.NoError(t, f.app.GetMessageLog(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	var detail struct {
		Data handlers.MessageLogResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &detail))
	assert.Equal(t, long, detail.Data.Content, "detail returns the full content")
}

func TestGetMessageLog_IncludesSenderAndHidesOtherOrgs(t *testing.T) {
	f := newMessageLogsFixture(t)
	m := f.createLogMessage(t, time.Hour, func(m *models.Message) { m.SentByUserID = &f.user.ID })

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, f.org.ID, f.user.ID)
	testutil.SetPathParam(req, "id", m.ID.String())
	require.NoError(t, f.app.GetMessageLog(req))
	var detail struct {
		Data handlers.MessageLogResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &detail))
	assert.Equal(t, f.user.FullName, detail.Data.SentByUserName)

	other := newMessageLogsFixture(t)
	req = testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, other.org.ID, other.user.ID)
	testutil.SetPathParam(req, "id", m.ID.String())
	require.NoError(t, other.app.GetMessageLog(req))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestListMessageLogs_RequiresPermission(t *testing.T) {
	f := newMessageLogsFixture(t)
	agentRole := testutil.CreateAgentRole(t, f.app.DB, f.org.ID)
	agent := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&agentRole.ID))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, f.org.ID, agent.ID)
	testutil.SetQueryParam(req, "from", time.Now().AddDate(0, 0, -1).Format(time.DateOnly))
	testutil.SetQueryParam(req, "to", time.Now().Format(time.DateOnly))
	require.NoError(t, f.app.ListMessageLogs(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}
