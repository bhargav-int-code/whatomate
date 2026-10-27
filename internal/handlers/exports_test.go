package handlers_test

import (
	"encoding/json"
	"os"
	"path/filepath"
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

type exportsFixture struct {
	app   *handlers.App
	queue *testutil.MockQueue
	org   *models.Organization
	admin *models.User
	role  *models.CustomRole
	root  string
}

func newExportsFixture(t *testing.T) *exportsFixture {
	t.Helper()
	q := testutil.NewMockQueue()
	app := newTestApp(t, withQueue(q))
	root := t.TempDir()
	app.Config.Storage.LocalPath = root
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
	return &exportsFixture{app: app, queue: q, org: org, admin: admin, role: role, root: root}
}

func (f *exportsFixture) requestExport(t *testing.T, userID uuid.UUID, body map[string]any) (int, handlers.ExportJobResponse) {
	t.Helper()
	req := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(req, f.org.ID, userID)
	require.NoError(t, f.app.RequestMessageLogExport(req))
	var resp struct {
		Data handlers.ExportJobResponse `json:"data"`
	}
	_ = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	return testutil.GetResponseStatusCode(req), resp.Data
}

func lastWeekExport() map[string]any {
	return map[string]any{
		"from":     time.Now().AddDate(0, 0, -7).Format(time.DateOnly),
		"to":       time.Now().Format(time.DateOnly),
		"status":   "failed",
		"phone":    "+91 98765 43210",
		"timezone": "Asia/Kolkata",
	}
}

// storeExport inserts an export row; with a file name, it also writes a file.
func (f *exportsFixture) storeExport(t *testing.T, userID uuid.UUID, status models.ExportStatus, fileName string, mutate func(*models.ExportJob)) models.ExportJob {
	t.Helper()
	job := models.ExportJob{
		ID: uuid.New(), OrganizationID: f.org.ID, UserID: userID, Type: models.ExportTypeMessageLogs,
		Filters: models.JSONB{}, Timezone: "UTC", Status: status,
	}
	if fileName != "" {
		rel := filepath.Join("exports", f.org.ID.String(), job.ID.String()+filepath.Ext(fileName))
		require.NoError(t, os.MkdirAll(filepath.Join(f.root, filepath.Dir(rel)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(f.root, rel), []byte("\xEF\xBB\xBFTime\n2026-10-24\n"), 0o640))
		job.FilePath, job.FileName = rel, fileName
	}
	if mutate != nil {
		mutate(&job)
	}
	require.NoError(t, f.app.DB.Create(&job).Error)
	return job
}

func TestRequestMessageLogExport_QueuesJob(t *testing.T) {
	f := newExportsFixture(t)

	status, job := f.requestExport(t, f.admin.ID, lastWeekExport())
	require.Equal(t, fasthttp.StatusOK, status)
	assert.Equal(t, models.ExportStatusQueued, job.Status)
	assert.Equal(t, "Asia/Kolkata", job.Timezone)
	assert.Equal(t, "failed", job.Filters["status"])
	assert.Equal(t, "919876543210", job.Filters["phone"], "phone is normalized like the logs page")

	require.Len(t, f.queue.ExportJobs, 1)
	assert.Equal(t, job.ID, f.queue.ExportJobs[0].ExportID)

	// Audit entries are written asynchronously.
	testutil.AssertEventually(t, func() bool {
		var audits int64
		f.app.DB.Model(&models.AuditLog{}).Where("organization_id = ? AND resource_id = ?", f.org.ID, job.ID).Count(&audits)
		return audits == 1
	}, 3*time.Second, "exports are audited")
}

func TestRequestMessageLogExport_Validation(t *testing.T) {
	f := newExportsFixture(t)
	tests := map[string]func(map[string]any){
		"bad timezone":   func(b map[string]any) { b["timezone"] = "Mars/Base" },
		"older than 90d": func(b map[string]any) { b["from"] = time.Now().AddDate(0, 0, -100).Format(time.DateOnly) },
		"invalid status": func(b map[string]any) { b["status"] = "bounced" },
		"missing range":  func(b map[string]any) { delete(b, "from") },
		"reversed range": func(b map[string]any) { b["to"] = time.Now().AddDate(0, 0, -10).Format(time.DateOnly) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			body := lastWeekExport()
			mutate(body)
			status, _ := f.requestExport(t, f.admin.ID, body)
			assert.Equal(t, fasthttp.StatusBadRequest, status)
		})
	}
	assert.Empty(t, f.queue.ExportJobs)
}

func TestRequestMessageLogExport_RequiresExportPermission(t *testing.T) {
	f := newExportsFixture(t)
	readOnly := testutil.CreateTestRoleWithKeys(t, f.app.DB, f.org.ID, "viewer", []string{"message_logs:read"})
	viewer := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&readOnly.ID))

	status, _ := f.requestExport(t, viewer.ID, lastWeekExport())
	assert.Equal(t, fasthttp.StatusForbidden, status)
}

func TestRequestMessageLogExport_ConcurrencyLimits(t *testing.T) {
	f := newExportsFixture(t)

	status, _ := f.requestExport(t, f.admin.ID, lastWeekExport())
	require.Equal(t, fasthttp.StatusOK, status)
	status, _ = f.requestExport(t, f.admin.ID, lastWeekExport())
	assert.Equal(t, fasthttp.StatusConflict, status, "one active export per user")

	second := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&f.role.ID))
	status, _ = f.requestExport(t, second.ID, lastWeekExport())
	require.Equal(t, fasthttp.StatusOK, status)

	third := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&f.role.ID))
	status, _ = f.requestExport(t, third.ID, lastWeekExport())
	assert.Equal(t, fasthttp.StatusConflict, status, "two active exports per organization")

	// A finished export frees the slot.
	require.NoError(t, f.app.DB.Model(&models.ExportJob{}).Where("user_id = ?", f.admin.ID).
		Update("status", models.ExportStatusCompleted).Error)
	status, _ = f.requestExport(t, third.ID, lastWeekExport())
	assert.Equal(t, fasthttp.StatusOK, status)
}

func TestListExports_OnlyOwn(t *testing.T) {
	f := newExportsFixture(t)
	mine := f.storeExport(t, f.admin.ID, models.ExportStatusCompleted, "message-logs_a.csv", nil)
	other := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&f.role.ID))
	f.storeExport(t, other.ID, models.ExportStatusCompleted, "message-logs_b.csv", nil)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, f.org.ID, f.admin.ID)
	require.NoError(t, f.app.ListExports(req))
	var resp struct {
		Data struct {
			Exports []handlers.ExportJobResponse `json:"exports"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	require.Len(t, resp.Data.Exports, 1)
	assert.Equal(t, mine.ID, resp.Data.Exports[0].ID)
}

func (f *exportsFixture) download(t *testing.T, userID, exportID uuid.UUID) *fasthttp.RequestCtx {
	t.Helper()
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, f.org.ID, userID)
	testutil.SetPathParam(req, "id", exportID.String())
	require.NoError(t, f.app.DownloadExport(req))
	return req.RequestCtx
}

func TestDownloadExport(t *testing.T) {
	f := newExportsFixture(t)
	done := f.storeExport(t, f.admin.ID, models.ExportStatusCompleted, "message-logs_2026-10-01_to_2026-10-24.csv", nil)

	ctx := f.download(t, f.admin.ID, done.ID)
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	assert.Equal(t, `attachment; filename="message-logs_2026-10-01_to_2026-10-24.csv"`, string(ctx.Response.Header.Peek("Content-Disposition")))
	assert.Equal(t, "text/csv; charset=utf-8", string(ctx.Response.Header.ContentType()))
	assert.Equal(t, "\xEF\xBB\xBFTime\n2026-10-24\n", string(ctx.Response.Body()))
	entries, _ := os.ReadDir(filepath.Join(f.root, "exports", f.org.ID.String()))
	assert.Len(t, entries, 1, "no compressed copy is cached next to the file")

	other := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&f.role.ID))
	assert.Equal(t, fasthttp.StatusNotFound, f.download(t, other.ID, done.ID).Response.StatusCode(), "exports are private to the requester")

	pending := f.storeExport(t, f.admin.ID, models.ExportStatusProcessing, "", nil)
	assert.Equal(t, fasthttp.StatusConflict, f.download(t, f.admin.ID, pending.ID).Response.StatusCode())

	// The requester can download an export from another of their
	// organizations: browser downloads don't carry the org-switch header.
	otherOrg := testutil.CreateTestOrganization(t, f.app.DB)
	elsewhere := f.storeExport(t, f.admin.ID, models.ExportStatusCompleted, "message-logs_b.csv", func(j *models.ExportJob) {
		j.OrganizationID = otherOrg.ID
	})
	assert.Equal(t, fasthttp.StatusOK, f.download(t, f.admin.ID, elsewhere.ID).Response.StatusCode())

	escape := f.storeExport(t, f.admin.ID, models.ExportStatusCompleted, "", func(j *models.ExportJob) {
		j.FilePath, j.FileName = "../../etc/passwd", "x.csv"
	})
	assert.Equal(t, fasthttp.StatusNotFound, f.download(t, f.admin.ID, escape.ID).Response.StatusCode(), "paths outside storage are refused")
}

func TestDeleteExport(t *testing.T) {
	f := newExportsFixture(t)
	done := f.storeExport(t, f.admin.ID, models.ExportStatusCompleted, "message-logs_a.csv", nil)
	running := f.storeExport(t, f.admin.ID, models.ExportStatusProcessing, "", nil)

	del := func(id uuid.UUID) int {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, f.org.ID, f.admin.ID)
		testutil.SetPathParam(req, "id", id.String())
		require.NoError(t, f.app.DeleteExport(req))
		return testutil.GetResponseStatusCode(req)
	}

	assert.Equal(t, fasthttp.StatusConflict, del(running.ID))
	assert.Equal(t, fasthttp.StatusOK, del(done.ID))
	_, err := os.Stat(filepath.Join(f.root, done.FilePath))
	assert.True(t, os.IsNotExist(err), "file removed")
	var count int64
	f.app.DB.Model(&models.ExportJob{}).Where("id = ?", done.ID).Count(&count)
	assert.Zero(t, count)
}

func TestSweepExports(t *testing.T) {
	f := newExportsFixture(t)
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	expired := f.storeExport(t, f.admin.ID, models.ExportStatusCompleted, "old.csv", func(j *models.ExportJob) { j.ExpiresAt = &past })
	fresh := f.storeExport(t, f.admin.ID, models.ExportStatusCompleted, "new.csv", func(j *models.ExportJob) { j.ExpiresAt = &future })
	staleBeat := time.Now().Add(-30 * time.Minute)
	stuck := f.storeExport(t, f.admin.ID, models.ExportStatusProcessing, "", func(j *models.ExportJob) { j.HeartbeatAt = &staleBeat })
	orphan := f.storeExport(t, f.admin.ID, models.ExportStatusQueued, "", func(j *models.ExportJob) { j.CreatedAt = time.Now().Add(-2 * time.Hour) })

	f.app.SweepExports()

	reload := func(id uuid.UUID) models.ExportJob {
		var j models.ExportJob
		require.NoError(t, f.app.DB.First(&j, "id = ?", id).Error)
		return j
	}
	assert.Equal(t, models.ExportStatusExpired, reload(expired.ID).Status)
	_, err := os.Stat(filepath.Join(f.root, expired.FilePath))
	assert.True(t, os.IsNotExist(err), "expired file deleted")
	assert.Equal(t, models.ExportStatusCompleted, reload(fresh.ID).Status)
	_, err = os.Stat(filepath.Join(f.root, fresh.FilePath))
	assert.NoError(t, err)
	assert.Equal(t, models.ExportStatusFailed, reload(stuck.ID).Status)
	assert.Equal(t, models.ExportStatusFailed, reload(orphan.ID).Status)
}
