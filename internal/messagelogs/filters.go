// Package messagelogs holds the message-log filters and the export writer.
// The list API and the export worker share the filters, so an export always
// contains exactly what the logs page showed.
package messagelogs

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

var (
	validStatuses = map[string]bool{
		string(models.MessageStatusPending): true, string(models.MessageStatusSent): true,
		string(models.MessageStatusDelivered): true, string(models.MessageStatusRead): true,
		string(models.MessageStatusFailed): true, string(models.MessageStatusReceived): true,
	}
	validDirections = map[string]bool{
		string(models.DirectionIncoming): true, string(models.DirectionOutgoing): true,
	}
	validSources = map[string]bool{
		string(models.MessageSourceAPI): true, string(models.MessageSourceMCP): true,
		string(models.MessageSourceAgent): true, string(models.MessageSourceChatbot): true,
		string(models.MessageSourceCampaign): true, string(models.MessageSourceAutomation): true,
		string(models.MessageSourcePhoneApp): true, string(models.MessageSourceIncoming): true,
		string(models.MessageSourceCall): true,
	}
)

// ErrInvalidFilter is returned by Validate for an unknown status, direction or
// source.
var ErrInvalidFilter = errors.New("invalid status, direction or source filter")

// Filters selects message log rows. Empty fields don't filter.
type Filters struct {
	From      time.Time
	To        time.Time
	Account   string
	Status    string
	Direction string
	Source    string
	Template  string
	// Phone is the customer number as digits only; see NormalizePhone.
	Phone string
}

// Validate checks the enumerated filters.
func (f Filters) Validate() error {
	if (f.Status != "" && !validStatuses[f.Status]) ||
		(f.Direction != "" && !validDirections[f.Direction]) ||
		(f.Source != "" && !validSources[f.Source]) {
		return ErrInvalidFilter
	}
	return nil
}

// NormalizePhone strips everything but digits, matching how contact phone
// numbers are stored ("+91 98765-43210" -> "919876543210").
func NormalizePhone(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// ResolveContacts returns the IDs of the organization's contacts with this
// phone number, including deleted ones, whose messages are still logged.
// Filtering messages by contact uses the per-contact index.
func ResolveContacts(db *gorm.DB, orgID uuid.UUID, phone string) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := db.Unscoped().Model(&models.Contact{}).
		Where("organization_id = ? AND phone_number = ?", orgID, phone).
		Pluck("id", &ids).Error
	return ids, err
}

// Apply adds the filters to a query on messages aliased as m. contactIDs is
// the result of ResolveContacts when f.Phone is set; the caller handles the
// no-match case, since an empty IN list matches nothing anyway.
func (f Filters) Apply(query *gorm.DB, orgID uuid.UUID, contactIDs []uuid.UUID) *gorm.DB {
	query = query.Where("m.organization_id = ? AND m.deleted_at IS NULL", orgID).
		Where("m.created_at >= ? AND m.created_at <= ?", f.From, f.To)
	if f.Account != "" {
		query = query.Where("m.whats_app_account = ?", f.Account)
	}
	if f.Status != "" {
		query = query.Where("m.status = ?", f.Status)
	}
	if f.Direction != "" {
		query = query.Where("m.direction = ?", f.Direction)
	}
	if f.Source != "" {
		query = query.Where("m.source = ?", f.Source)
	}
	if f.Template != "" {
		query = query.Where("m.template_name = ?", f.Template)
	}
	if f.Phone != "" {
		query = query.Where("m.contact_id IN ?", contactIDs)
	}
	return query
}

// JSONB encodes the filters for storing on an export job.
func (f Filters) JSONB() models.JSONB {
	return models.JSONB{
		"from":      f.From.UTC().Format(time.RFC3339Nano),
		"to":        f.To.UTC().Format(time.RFC3339Nano),
		"account":   f.Account,
		"status":    f.Status,
		"direction": f.Direction,
		"source":    f.Source,
		"template":  f.Template,
		"phone":     f.Phone,
	}
}

// FiltersFromJSONB decodes filters stored by JSONB.
func FiltersFromJSONB(j models.JSONB) (Filters, error) {
	str := func(key string) string {
		s, _ := j[key].(string)
		return s
	}
	from, err := time.Parse(time.RFC3339Nano, str("from"))
	if err != nil {
		return Filters{}, errors.New("export filters have no valid from time")
	}
	to, err := time.Parse(time.RFC3339Nano, str("to"))
	if err != nil {
		return Filters{}, errors.New("export filters have no valid to time")
	}
	f := Filters{
		From:      from,
		To:        to,
		Account:   str("account"),
		Status:    str("status"),
		Direction: str("direction"),
		Source:    str("source"),
		Template:  str("template"),
		Phone:     str("phone"),
	}
	return f, f.Validate()
}
