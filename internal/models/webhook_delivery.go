package models

import (
	"time"

	"github.com/google/uuid"
)

// WebhookDeliveryStatus is the state of one outbound webhook delivery
type WebhookDeliveryStatus string

const (
	WebhookDeliveryProcessing WebhookDeliveryStatus = "processing" // first attempt or a retry in flight
	WebhookDeliverySuccess    WebhookDeliveryStatus = "success"    // an attempt got a 2xx response
	WebhookDeliveryFailed     WebhookDeliveryStatus = "failed"     // all attempts failed, or the delivery was cut short
)

// WebhookDelivery records one event sent to one webhook, including a snapshot
// of the request and every attempt. Rows are hard-deleted by the admin purge,
// so there is no soft delete.
type WebhookDelivery struct {
	ID             uuid.UUID             `gorm:"type:uuid;primary_key;default:gen_random_uuid()" json:"id"`
	CreatedAt      time.Time             `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt      time.Time             `gorm:"autoUpdateTime" json:"updated_at"`
	OrganizationID uuid.UUID             `gorm:"type:uuid;not null" json:"organization_id"`
	WebhookID      uuid.UUID             `gorm:"type:uuid;not null" json:"webhook_id"`
	WebhookName    string                `gorm:"size:255" json:"webhook_name"` // copied so logs survive renames and deletes
	Event          string                `gorm:"size:50;not null" json:"event"`
	ReferenceID    string                `gorm:"size:255" json:"reference_id"` // message, contact or transfer the event is about
	ContactPhone   string                `gorm:"size:50" json:"contact_phone"`
	Status         WebhookDeliveryStatus `gorm:"size:20;not null" json:"status"`
	AttemptCount   int                   `gorm:"default:0" json:"attempt_count"`
	ResponseStatus int                   `gorm:"default:0" json:"response_status"` // last attempt's HTTP status, 0 if no response
	FailureReason  string                `gorm:"type:text" json:"failure_reason"`
	DurationMs     int                   `gorm:"default:0" json:"duration_ms"` // last attempt
	RequestURL     string                `gorm:"type:text" json:"request_url"`
	RequestHeaders JSONB                 `gorm:"type:jsonb" json:"request_headers"`
	RequestBody    string                `gorm:"type:text" json:"request_body"`  // exact bytes sent (and signed)
	ResponseBody   string                `gorm:"type:text" json:"response_body"` // start of the last response body
	// Attempts is [{attempt, at, status_code, duration_ms, error}], one entry
	// per attempt in order.
	Attempts    JSONBArray `gorm:"type:jsonb;default:'[]'" json:"attempts"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func (WebhookDelivery) TableName() string {
	return "webhook_deliveries"
}
