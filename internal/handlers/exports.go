package handlers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/messagelogs"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const (
	// Concurrent export limits, so a burst of exports can't load the
	// database: one in flight per user, two per organization.
	maxActiveExportsPerUser = 1
	maxActiveExportsPerOrg  = 2
	// exportsListLimit is how many of a user's exports the list returns.
	exportsListLimit = 50
	// exportSweepInterval is how often expired files are deleted and stuck
	// exports closed out.
	exportSweepInterval = time.Hour
	// exportProcessingStaleAfter marks a processing export whose worker
	// stopped sending heartbeats (about every 30s) as failed.
	exportProcessingStaleAfter = 10 * time.Minute
	// exportQueuedStaleAfter marks an export no worker picked up as failed.
	exportQueuedStaleAfter = time.Hour
)

var (
	errExportLimitUser = errors.New("you already have an export in progress; wait for it to finish")
	errExportLimitOrg  = errors.New("your organization already has 2 exports in progress; try again shortly")
)

// MessageLogExportRequest is the body of POST /api/message-logs/exports: the
// logs page filters plus the viewer's timezone.
type MessageLogExportRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Account   string `json:"account"`
	Status    string `json:"status"`
	Direction string `json:"direction"`
	Source    string `json:"source"`
	Template  string `json:"template"`
	Phone     string `json:"phone"`
	// Timezone is an IANA zone (e.g. Asia/Kolkata) for the file's times.
	Timezone string `json:"timezone"`
}

// ExportJobResponse is an export as listed on the Downloads page
type ExportJobResponse struct {
	ID          uuid.UUID           `json:"id"`
	Type        models.ExportType   `json:"type"`
	Status      models.ExportStatus `json:"status"`
	Filters     models.JSONB        `json:"filters"`
	Timezone    string              `json:"timezone"`
	RowCount    int64               `json:"row_count"`
	FileSize    int64               `json:"file_size"`
	FileName    string              `json:"file_name"`
	Error       string              `json:"error,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	StartedAt   *time.Time          `json:"started_at"`
	CompletedAt *time.Time          `json:"completed_at"`
	ExpiresAt   *time.Time          `json:"expires_at"`
}

func exportJobToResponse(j models.ExportJob) ExportJobResponse {
	return ExportJobResponse{
		ID:          j.ID,
		Type:        j.Type,
		Status:      j.Status,
		Filters:     j.Filters,
		Timezone:    j.Timezone,
		RowCount:    j.RowCount,
		FileSize:    j.FileSize,
		FileName:    j.FileName,
		Error:       j.Error,
		CreatedAt:   j.CreatedAt,
		StartedAt:   j.StartedAt,
		CompletedAt: j.CompletedAt,
		ExpiresAt:   j.ExpiresAt,
	}
}

// RequestMessageLogExport queues an export of the message logs matching the
// given filters. A worker generates the file; the requester is notified over
// WebSocket and downloads it from the Downloads page.
func (a *App) RequestMessageLogExport(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceMessageLogs, models.ActionExport)
	if err != nil {
		return nil
	}

	var req MessageLogExportRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	from, to, errMsg := parseLogRange(req.From, req.To)
	if errMsg != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, errMsg, nil, "")
	}
	filters := messagelogs.Filters{
		From:      from,
		To:        to,
		Account:   req.Account,
		Status:    req.Status,
		Direction: req.Direction,
		Source:    req.Source,
		Template:  req.Template,
		Phone:     messagelogs.NormalizePhone(req.Phone),
	}
	if err := filters.Validate(); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid status, direction or source filter", nil, "")
	}
	loc, err := messagelogs.LoadLocation(req.Timezone)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid timezone", nil, "")
	}

	job := models.ExportJob{
		ID:             uuid.New(),
		OrganizationID: orgID,
		UserID:         userID,
		Type:           models.ExportTypeMessageLogs,
		Filters:        filters.JSONB(),
		Timezone:       loc.String(),
		Status:         models.ExportStatusQueued,
	}
	if err := a.createExportJob(&job); err != nil {
		if errors.Is(err, errExportLimitUser) || errors.Is(err, errExportLimitOrg) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, capitalize(err.Error()), nil, "")
		}
		a.Log.Error("Failed to create export", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start export", nil, "")
	}

	if err := a.Queue.EnqueueExport(context.Background(), &queue.ExportJob{ExportID: job.ID}); err != nil {
		a.Log.Error("Failed to enqueue export", "error", err, "export_id", job.ID)
		a.DB.Model(&job).Updates(map[string]any{"status": models.ExportStatusFailed, "error": "Could not queue the export"})
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start export", nil, "")
	}

	a.logAudit(orgID, userID, models.ResourceMessageLogs, job.ID, models.AuditActionCreated, nil,
		map[string]any{"export": string(job.Type), "filters": job.Filters, "timezone": job.Timezone})

	return r.SendEnvelope(exportJobToResponse(job))
}

// createExportJob inserts the job if the user and organization are under
// their concurrent-export limits. A per-organization advisory lock makes the
// check and insert atomic against simultaneous requests.
func (a *App) createExportJob(job *models.ExportJob) error {
	return a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "exports:"+job.OrganizationID.String()).Error; err != nil {
			return err
		}
		active := []models.ExportStatus{models.ExportStatusQueued, models.ExportStatusProcessing}

		var userActive, orgActive int64
		if err := tx.Model(&models.ExportJob{}).
			Where("organization_id = ? AND user_id = ? AND status IN ?", job.OrganizationID, job.UserID, active).
			Count(&userActive).Error; err != nil {
			return err
		}
		if userActive >= maxActiveExportsPerUser {
			return errExportLimitUser
		}
		if err := tx.Model(&models.ExportJob{}).
			Where("organization_id = ? AND status IN ?", job.OrganizationID, active).
			Count(&orgActive).Error; err != nil {
			return err
		}
		if orgActive >= maxActiveExportsPerOrg {
			return errExportLimitOrg
		}
		return tx.Create(job).Error
	})
}

// ListExports returns the current user's exports, newest first.
func (a *App) ListExports(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	var jobs []models.ExportJob
	if err := a.DB.Where("organization_id = ? AND user_id = ?", orgID, userID).
		Order("created_at DESC").Limit(exportsListLimit).Find(&jobs).Error; err != nil {
		a.Log.Error("Failed to list exports", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list exports", nil, "")
	}

	out := make([]ExportJobResponse, len(jobs))
	for i, j := range jobs {
		out[i] = exportJobToResponse(j)
	}
	return r.SendEnvelope(map[string]any{"exports": out})
}

// findOwnExport loads an export belonging to the requesting user, sending a
// 404 otherwise. Exports are private to whoever requested them.
func (a *App) findOwnExport(r *fastglue.Request) (*models.ExportJob, error) {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
		return nil, errEnvelopeSent
	}
	id, err := parsePathUUID(r, "id", "export")
	if err != nil {
		return nil, err
	}
	var job models.ExportJob
	if err := a.DB.Where("id = ? AND organization_id = ? AND user_id = ?", id, orgID, userID).First(&job).Error; err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusNotFound, "Export not found", nil, "")
		return nil, errEnvelopeSent
	}
	return &job, nil
}

// exportFilePath resolves an export's stored relative path inside the storage
// root, refusing anything that escapes it or is a symlink.
func (a *App) exportFilePath(job *models.ExportJob) (string, error) {
	root, err := filepath.Abs(a.getMediaStoragePath())
	if err != nil {
		return "", err
	}
	full, err := filepath.Abs(filepath.Join(root, filepath.Clean(job.FilePath)))
	if err != nil || !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", errors.New("invalid export path")
	}
	info, err := os.Lstat(full)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("invalid export file")
	}
	return full, nil
}

// DownloadExport streams a completed export file to its requester.
func (a *App) DownloadExport(r *fastglue.Request) error {
	job, err := a.findOwnExport(r)
	if err != nil {
		return nil
	}
	if job.Status != models.ExportStatusCompleted {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Export is not ready to download", nil, "")
	}
	path, err := a.exportFilePath(job)
	if err != nil {
		a.Log.Warn("Export file unavailable", "error", err, "export_id", job.ID)
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Export file not found", nil, "")
	}

	contentType := "text/csv; charset=utf-8"
	if strings.HasSuffix(job.FileName, ".zip") {
		contentType = "application/zip"
	}
	// Streamed from disk: exports can be hundreds of MB. Uncompressed, because
	// fasthttp's compressing file server caches a .gz copy next to the file,
	// which would outlive the export's expiry.
	r.RequestCtx.Response.Header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, job.FileName))
	r.RequestCtx.Response.Header.Set("Cache-Control", "private, no-store")
	fasthttp.ServeFileUncompressed(r.RequestCtx, path)
	r.RequestCtx.SetContentType(contentType)
	return nil
}

// DeleteExport removes a finished export and its file.
func (a *App) DeleteExport(r *fastglue.Request) error {
	job, err := a.findOwnExport(r)
	if err != nil {
		return nil
	}
	if job.Status == models.ExportStatusQueued || job.Status == models.ExportStatusProcessing {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Export is still being generated", nil, "")
	}
	a.removeExportFile(job)
	if err := a.DB.Delete(&models.ExportJob{}, "id = ?", job.ID).Error; err != nil {
		a.Log.Error("Failed to delete export", "error", err, "export_id", job.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete export", nil, "")
	}
	return r.SendEnvelope(map[string]string{"message": "Export deleted"})
}

func (a *App) removeExportFile(job *models.ExportJob) {
	if job.FilePath == "" {
		return
	}
	path, err := a.exportFilePath(job)
	if err != nil {
		return // already gone
	}
	if err := os.Remove(path); err != nil {
		a.Log.Warn("Failed to remove export file", "error", err, "export_id", job.ID)
	}
}

// StartExportUpdatesSubscriber relays workers' export updates to the
// requester over WebSocket.
func (a *App) StartExportUpdatesSubscriber(ctx context.Context) error {
	if a.WSHub == nil {
		return nil
	}
	subscriber := queue.NewSubscriber(a.Redis, a.Log)
	return subscriber.SubscribeExportUpdates(ctx, func(update *queue.ExportUpdate) {
		a.WSHub.BroadcastToUser(update.OrganizationID, update.UserID, websocket.WSMessage{
			Type:    websocket.TypeExportUpdate,
			Payload: update,
		})
	})
}

// SweepExports deletes expired export files and fails exports that are stuck:
// processing with a dead worker, or queued with no worker to pick them up.
func (a *App) SweepExports() {
	now := time.Now()

	var expired []models.ExportJob
	if err := a.DB.Where("status = ? AND expires_at < ?", models.ExportStatusCompleted, now).
		Find(&expired).Error; err != nil {
		a.Log.Error("Failed to find expired exports", "error", err)
	}
	for i := range expired {
		a.removeExportFile(&expired[i])
		a.DB.Model(&expired[i]).Updates(map[string]any{"status": models.ExportStatusExpired, "file_path": ""})
	}

	a.DB.Model(&models.ExportJob{}).
		Where("status = ? AND (heartbeat_at IS NULL OR heartbeat_at < ?)", models.ExportStatusProcessing, now.Add(-exportProcessingStaleAfter)).
		Updates(map[string]any{"status": models.ExportStatusFailed, "error": "The export was interrupted. Please request it again.", "completed_at": now})

	a.DB.Model(&models.ExportJob{}).
		Where("status = ? AND created_at < ?", models.ExportStatusQueued, now.Add(-exportQueuedStaleAfter)).
		Updates(map[string]any{"status": models.ExportStatusFailed, "error": "No worker picked up this export. Please try again later.", "completed_at": now})
}

// StartExportSweeper runs SweepExports now and then every exportSweepInterval
// until ctx is cancelled.
func (a *App) StartExportSweeper(ctx context.Context) {
	a.SweepExports()
	ticker := time.NewTicker(exportSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.SweepExports()
		}
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
