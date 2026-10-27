package queue

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

// JobType represents the type of job
type JobType string

const (
	// JobTypeRecipient is for processing a single recipient message
	JobTypeRecipient JobType = "recipient"
	// JobTypeExport is for generating an export file
	JobTypeExport JobType = "export"
)

// ExportJob asks a worker to generate the file for one export_jobs row. The
// row holds everything else (type, filters, requester), so a retried or
// duplicated job always works from the stored state.
type ExportJob struct {
	ExportID uuid.UUID `json:"export_id"`
}

// RecipientJob represents a single recipient message job
type RecipientJob struct {
	CampaignID     uuid.UUID    `json:"campaign_id"`
	RecipientID    uuid.UUID    `json:"recipient_id"`
	OrganizationID uuid.UUID    `json:"organization_id"`
	PhoneNumber    string       `json:"phone_number"`
	RecipientName  string       `json:"recipient_name"`
	TemplateParams models.JSONB `json:"template_params"`
	// HeaderParams holds the TEXT-header variable's value (Meta restricts
	// these headers to one variable). Sent separately from TemplateParams to
	// avoid positional-key collisions between header and body.
	HeaderParams models.JSONB `json:"header_params"`
	EnqueuedAt   time.Time    `json:"enqueued_at"`
}

// Queue defines the interface for job queue operations
type Queue interface {
	// EnqueueRecipient adds a single recipient job to the queue
	EnqueueRecipient(ctx context.Context, job *RecipientJob) error

	// EnqueueRecipients adds multiple recipient jobs to the queue
	EnqueueRecipients(ctx context.Context, jobs []*RecipientJob) error

	// EnqueueExport adds an export job to the exports stream
	EnqueueExport(ctx context.Context, job *ExportJob) error

	// Close closes the queue connection
	Close() error
}

// JobHandler handles different job types
type JobHandler interface {
	HandleRecipientJob(ctx context.Context, job *RecipientJob) error
}

// ExportJobHandler is implemented by handlers that also process export jobs
// (the export consumer requires it).
type ExportJobHandler interface {
	HandleExportJob(ctx context.Context, job *ExportJob) error
}

// Consumer defines the interface for consuming jobs from the queue
type Consumer interface {
	// Consume starts consuming jobs from the queue
	// Returns when context is cancelled
	Consume(ctx context.Context, handler JobHandler) error

	// Close closes the consumer connection
	Close() error
}
