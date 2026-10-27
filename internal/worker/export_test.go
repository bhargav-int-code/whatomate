package worker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/messagelogs"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type exportWorkerFixture struct {
	w       *Worker
	org     *models.Organization
	user    *models.User
	contact *models.Contact
}

func newExportWorkerFixture(t *testing.T) *exportWorkerFixture {
	t.Helper()
	w := testWorker(t)
	if w.Publisher == nil {
		t.Skip("TEST_REDIS_URL not set, skipping test")
	}
	w.Config = &config.Config{Storage: config.StorageConfig{LocalPath: t.TempDir()}}
	org := testutil.CreateTestOrganization(t, w.DB)
	user := testutil.CreateTestUser(t, w.DB, org.ID)
	contact := testutil.CreateTestContact(t, w.DB, org.ID)
	return &exportWorkerFixture{w: w, org: org, user: user, contact: contact}
}

func (f *exportWorkerFixture) message(t *testing.T, createdAt time.Time) models.Message {
	t.Helper()
	m := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: createdAt}, OrganizationID: f.org.ID,
		WhatsAppAccount: "Main", ContactID: f.contact.ID, Direction: models.DirectionOutgoing,
		MessageType: models.MessageTypeText, Content: "hello", Status: models.MessageStatusSent,
	}
	require.NoError(t, f.w.DB.Create(&m).Error)
	return m
}

// exportJob stores an export row; mutate adjusts it first.
func (f *exportWorkerFixture) exportJob(t *testing.T, mutate func(*models.ExportJob)) models.ExportJob {
	t.Helper()
	filters := messagelogs.Filters{From: time.Now().Add(-24 * time.Hour), To: time.Now().Add(time.Hour)}
	job := models.ExportJob{
		ID: uuid.New(), OrganizationID: f.org.ID, UserID: f.user.ID, Type: models.ExportTypeMessageLogs,
		Filters: filters.JSONB(), Timezone: "Asia/Kolkata", Status: models.ExportStatusQueued,
	}
	if mutate != nil {
		mutate(&job)
	}
	require.NoError(t, f.w.DB.Create(&job).Error)
	return job
}

func (f *exportWorkerFixture) reload(t *testing.T, id uuid.UUID) models.ExportJob {
	t.Helper()
	var job models.ExportJob
	require.NoError(t, f.w.DB.First(&job, "id = ?", id).Error)
	return job
}

func TestHandleExportJob_CompletesAndNotifies(t *testing.T) {
	f := newExportWorkerFixture(t)
	f.message(t, time.Now().Add(-time.Hour))
	job := f.exportJob(t, nil)

	var mu sync.Mutex
	var updates []*queue.ExportUpdate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := queue.NewSubscriber(f.w.Redis, f.w.Log)
	require.NoError(t, sub.SubscribeExportUpdates(ctx, func(u *queue.ExportUpdate) {
		mu.Lock()
		updates = append(updates, u)
		mu.Unlock()
	}))
	defer sub.Close()

	require.NoError(t, f.w.HandleExportJob(context.Background(), &queue.ExportJob{ExportID: job.ID}))

	got := f.reload(t, job.ID)
	assert.Equal(t, models.ExportStatusCompleted, got.Status)
	assert.EqualValues(t, 1, got.RowCount)
	assert.Positive(t, got.FileSize)
	assert.Contains(t, got.FileName, ".csv")
	require.NotNil(t, got.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(exportFileRetention), *got.ExpiresAt, time.Minute)
	_, err := os.Stat(filepath.Join(f.w.storageRoot(), got.FilePath))
	assert.NoError(t, err, "file is on disk")

	testutil.AssertEventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(updates) == 1 && updates[0].ExportID == job.ID && updates[0].Status == "completed" && updates[0].UserID == f.user.ID
	}, 3*time.Second, "the API is told the export is ready")
}

func TestHandleExportJob_RedeliveryDoesNotRunTwice(t *testing.T) {
	f := newExportWorkerFixture(t)
	job := f.exportJob(t, nil)
	require.NoError(t, f.w.HandleExportJob(context.Background(), &queue.ExportJob{ExportID: job.ID}))
	first := f.reload(t, job.ID)

	require.NoError(t, f.w.HandleExportJob(context.Background(), &queue.ExportJob{ExportID: job.ID}))
	again := f.reload(t, job.ID)
	assert.Equal(t, models.ExportStatusCompleted, again.Status)
	assert.True(t, first.CompletedAt.Equal(*again.CompletedAt), "a completed export is never regenerated")
}

func TestHandleExportJob_ClaimRules(t *testing.T) {
	f := newExportWorkerFixture(t)

	live := f.exportJob(t, func(j *models.ExportJob) {
		j.Status = models.ExportStatusProcessing
		now := time.Now()
		j.HeartbeatAt = &now
	})
	require.NoError(t, f.w.HandleExportJob(context.Background(), &queue.ExportJob{ExportID: live.ID}))
	assert.Equal(t, models.ExportStatusProcessing, f.reload(t, live.ID).Status, "another worker is still writing it")

	stale := f.exportJob(t, func(j *models.ExportJob) {
		j.Status = models.ExportStatusProcessing
		old := time.Now().Add(-10 * time.Minute)
		j.HeartbeatAt = &old
	})
	require.NoError(t, f.w.HandleExportJob(context.Background(), &queue.ExportJob{ExportID: stale.ID}))
	assert.Equal(t, models.ExportStatusCompleted, f.reload(t, stale.ID).Status, "an abandoned export is taken over")

	require.NoError(t, f.w.HandleExportJob(context.Background(), &queue.ExportJob{ExportID: uuid.New()}), "a deleted export is skipped")
}

func TestHandleExportJob_RecordsFailures(t *testing.T) {
	f := newExportWorkerFixture(t)

	bad := f.exportJob(t, func(j *models.ExportJob) { j.Filters["status"] = "bounced" })
	require.NoError(t, f.w.HandleExportJob(context.Background(), &queue.ExportJob{ExportID: bad.ID}))
	got := f.reload(t, bad.ID)
	assert.Equal(t, models.ExportStatusFailed, got.Status)
	assert.NotEmpty(t, got.Error)
	assert.Empty(t, got.FilePath)

	interrupted := f.exportJob(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, f.w.HandleExportJob(ctx, &queue.ExportJob{ExportID: interrupted.ID}))
	got = f.reload(t, interrupted.ID)
	assert.Equal(t, models.ExportStatusFailed, got.Status)
	assert.Contains(t, got.Error, "interrupted")
}

func TestHandleExportJob_FreezesRangeAtRequestTime(t *testing.T) {
	f := newExportWorkerFixture(t)
	f.message(t, time.Now().Add(-2*time.Hour))
	job := f.exportJob(t, func(j *models.ExportJob) { j.CreatedAt = time.Now().Add(-time.Hour) })
	f.message(t, time.Now().Add(-time.Minute)) // after the request

	require.NoError(t, f.w.HandleExportJob(context.Background(), &queue.ExportJob{ExportID: job.ID}))
	assert.EqualValues(t, 1, f.reload(t, job.ID).RowCount)
}
