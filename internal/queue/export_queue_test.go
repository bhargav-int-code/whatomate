package queue_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type exportHandler struct {
	mockHandler
	mu      sync.Mutex
	exports []*queue.ExportJob
}

func (h *exportHandler) HandleExportJob(_ context.Context, job *queue.ExportJob) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.exports = append(h.exports, job)
	return nil
}

func (h *exportHandler) getExports() []*queue.ExportJob {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*queue.ExportJob(nil), h.exports...)
}

func cleanExportStream(t *testing.T) {
	t.Helper()
	client := skipIfNoRedis(t)
	ctx := context.Background()
	client.Del(ctx, queue.ExportStreamName)
	t.Cleanup(func() {
		client.Del(ctx, queue.ExportStreamName)
		client.XGroupDestroy(ctx, queue.ExportStreamName, queue.ExportConsumerGroup)
	})
}

func TestExportStream_DeliversToExportConsumerOnly(t *testing.T) {
	client := skipIfNoRedis(t)
	cleanStream(t, client)
	cleanExportStream(t)
	log := testutil.NopLogger()
	ctx := testutil.TestContextWithTimeout(t, 10*time.Second)

	q := queue.NewRedisQueue(client, log)
	job := &queue.ExportJob{ExportID: uuid.New()}
	require.NoError(t, q.EnqueueExport(ctx, job))

	// The export lands on its own stream, not the campaigns stream.
	assert.EqualValues(t, 1, client.XLen(ctx, queue.ExportStreamName).Val())
	assert.EqualValues(t, 0, client.XLen(ctx, queue.StreamName).Val())

	consumer, err := queue.NewRedisExportConsumer(client, log)
	require.NoError(t, err)
	handler := &exportHandler{}
	consumeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = consumer.Consume(consumeCtx, handler) }()

	testutil.AssertEventually(t, func() bool { return len(handler.getExports()) == 1 }, 8*time.Second,
		"export consumer should receive the job")
	assert.Equal(t, job.ExportID, handler.getExports()[0].ExportID)
	assert.Empty(t, handler.getJobs(), "no recipient jobs involved")
}
