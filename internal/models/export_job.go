package models

import (
	"time"

	"github.com/google/uuid"
)

// ExportType names what an export job produces
type ExportType string

const (
	ExportTypeMessageLogs ExportType = "message_logs"
)

// ExportStatus is the lifecycle state of an export job
type ExportStatus string

const (
	ExportStatusQueued     ExportStatus = "queued"     // waiting for a worker
	ExportStatusProcessing ExportStatus = "processing" // a worker is writing the file
	ExportStatusCompleted  ExportStatus = "completed"  // file ready to download
	ExportStatusFailed     ExportStatus = "failed"     // generation failed or was interrupted
	ExportStatusExpired    ExportStatus = "expired"    // file deleted after its retention period
)

// ExportJob is a background export requested by a user. Workers generate the
// file on local disk; the requester downloads it until it expires. Rows are
// hard-deleted, so there is no soft delete.
type ExportJob struct {
	ID             uuid.UUID    `gorm:"type:uuid;primary_key;default:gen_random_uuid()" json:"id"`
	CreatedAt      time.Time    `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt      time.Time    `gorm:"autoUpdateTime" json:"updated_at"`
	OrganizationID uuid.UUID    `gorm:"type:uuid;not null" json:"organization_id"`
	UserID         uuid.UUID    `gorm:"type:uuid;not null" json:"user_id"`
	Type           ExportType   `gorm:"size:50;not null" json:"type"`
	Filters        JSONB        `gorm:"type:jsonb;default:'{}'" json:"filters"`
	Timezone       string       `gorm:"size:64" json:"timezone"` // IANA zone the file's times are written in
	Status         ExportStatus `gorm:"size:20;not null" json:"status"`
	RowCount       int64        `gorm:"default:0" json:"row_count"`
	FileSize       int64        `gorm:"default:0" json:"file_size"`
	FileName       string       `gorm:"size:255" json:"file_name"`
	FilePath       string       `gorm:"type:text" json:"-"` // relative to the storage root
	Error          string       `gorm:"type:text" json:"error,omitempty"`
	// HeartbeatAt is refreshed while a worker writes the file, so a redelivered
	// job can tell a live export from one whose worker died.
	HeartbeatAt *time.Time `json:"-"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

func (ExportJob) TableName() string {
	return "export_jobs"
}
