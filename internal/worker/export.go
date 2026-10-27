package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shridarpatil/whatomate/internal/messagelogs"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"gorm.io/gorm"
)

const (
	// exportFileRetention is how long a finished export can be downloaded.
	exportFileRetention = 7 * 24 * time.Hour
	// exportStaleHeartbeat is when a "processing" export is treated as
	// abandoned and may be claimed by another worker. Heartbeats are sent about
	// every 30 seconds.
	exportStaleHeartbeat = 2 * time.Minute
	// exportsDir is where export files live, under the storage root.
	exportsDir = "exports"
)

// Ensure Worker processes export jobs
var _ queue.ExportJobHandler = (*Worker)(nil)

// HandleExportJob generates the file for one export. It always returns nil:
// failures are recorded on the export row for the user to see (they can
// request it again), rather than being retried by the queue.
func (w *Worker) HandleExportJob(ctx context.Context, job *queue.ExportJob) error {
	export, claimed, err := w.claimExport(job)
	if err != nil {
		w.Log.Error("Failed to claim export", "error", err, "export_id", job.ExportID)
		return nil
	}
	if !claimed {
		// Already done, running elsewhere with a live heartbeat, or deleted.
		w.Log.Info("Skipping export not available to claim", "export_id", job.ExportID)
		return nil
	}

	w.Log.Info("Generating export", "export_id", export.ID, "type", export.Type)
	result, genErr := w.generateExport(ctx, export)

	now := time.Now()
	update := &queue.ExportUpdate{ExportID: export.ID, OrganizationID: export.OrganizationID, UserID: export.UserID}
	fields := map[string]any{"completed_at": now, "heartbeat_at": nil}
	if genErr != nil {
		reason := genErr.Error()
		if ctx.Err() != nil {
			reason = "The export was interrupted by a server restart. Please request it again."
		}
		w.Log.Error("Export failed", "error", genErr, "export_id", export.ID)
		fields["status"] = models.ExportStatusFailed
		fields["error"] = reason
		update.Status, update.Error = string(models.ExportStatusFailed), reason
	} else {
		fields["status"] = models.ExportStatusCompleted
		fields["row_count"] = result.RowCount
		fields["file_size"] = result.FileSize
		fields["file_name"] = result.FileName
		fields["file_path"] = result.FilePath
		fields["expires_at"] = now.Add(exportFileRetention)
		update.Status, update.FileName, update.RowCount = string(models.ExportStatusCompleted), result.FileName, result.RowCount
		w.Log.Info("Export completed", "export_id", export.ID, "rows", result.RowCount, "bytes", result.FileSize)
	}

	// Use a fresh context: the job's may be cancelled (shutdown), and the
	// outcome must still be recorded.
	saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.DB.WithContext(saveCtx).Model(&models.ExportJob{}).Where("id = ?", export.ID).Updates(fields).Error; err != nil {
		w.Log.Error("Failed to record export result", "error", err, "export_id", export.ID)
		if genErr == nil {
			// The row can't point at the file, so don't leave it behind.
			_ = os.Remove(filepath.Join(w.storageRoot(), result.FilePath))
		}
		return nil
	}
	if err := w.Publisher.PublishExportUpdate(saveCtx, update); err != nil {
		w.Log.Warn("Failed to publish export update", "error", err, "export_id", export.ID)
	}
	return nil
}

// claimExport atomically marks the export as processing by this worker. A
// queued export can be claimed; so can a processing one whose heartbeat went
// stale (its worker died). Anything else is left alone, so a redelivered job
// never runs twice at once.
func (w *Worker) claimExport(job *queue.ExportJob) (*models.ExportJob, bool, error) {
	now := time.Now()
	result := w.DB.Model(&models.ExportJob{}).
		Where("id = ? AND (status = ? OR (status = ? AND heartbeat_at < ?))",
			job.ExportID, models.ExportStatusQueued, models.ExportStatusProcessing, now.Add(-exportStaleHeartbeat)).
		Updates(map[string]any{"status": models.ExportStatusProcessing, "started_at": now, "heartbeat_at": now})
	if result.Error != nil {
		return nil, false, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, false, nil
	}
	var export models.ExportJob
	if err := w.DB.First(&export, "id = ?", job.ExportID).Error; err != nil {
		return nil, false, err
	}
	return &export, true, nil
}

func (w *Worker) generateExport(ctx context.Context, export *models.ExportJob) (messagelogs.ExportResult, error) {
	if export.Type != models.ExportTypeMessageLogs {
		return messagelogs.ExportResult{}, fmt.Errorf("unsupported export type %q", export.Type)
	}

	filters, err := messagelogs.FiltersFromJSONB(export.Filters)
	if err != nil {
		return messagelogs.ExportResult{}, err
	}
	// Freeze the range at request time, so the file reflects what existed when
	// the user asked rather than when the worker got to it.
	if filters.To.After(export.CreatedAt) {
		filters.To = export.CreatedAt
	}
	loc, err := messagelogs.LoadLocation(export.Timezone)
	if err != nil {
		return messagelogs.ExportResult{}, err
	}

	return messagelogs.Export(ctx, messagelogs.ExportOptions{
		DB:          w.DB,
		OrgID:       export.OrganizationID,
		Filters:     filters,
		Location:    loc,
		MaskPhones:  w.orgMasksPhones(export.OrganizationID),
		StorageRoot: w.storageRoot(),
		RelDir:      filepath.Join(exportsDir, export.OrganizationID.String()),
		BaseName:    export.ID.String(),
		Heartbeat:   func() { w.exportHeartbeat(export) },
	})
}

func (w *Worker) exportHeartbeat(export *models.ExportJob) {
	if err := w.DB.Model(&models.ExportJob{}).Where("id = ?", export.ID).
		Update("heartbeat_at", time.Now()).Error; err != nil {
		w.Log.Warn("Failed to record export heartbeat", "error", err, "export_id", export.ID)
	}
}

// orgMasksPhones reports the organization's mask_phone_numbers setting.
func (w *Worker) orgMasksPhones(orgID any) bool {
	var org models.Organization
	if err := w.DB.Select("settings").Where("id = ?", orgID).First(&org).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			w.Log.Warn("Failed to load organization settings for export", "error", err)
		}
		return false
	}
	masked, _ := org.Settings["mask_phone_numbers"].(bool)
	return masked
}

func (w *Worker) storageRoot() string {
	if w.Config != nil && w.Config.Storage.LocalPath != "" {
		return w.Config.Storage.LocalPath
	}
	return "./uploads"
}
