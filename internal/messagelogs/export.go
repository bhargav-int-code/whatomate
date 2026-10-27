package messagelogs

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata" // export times are written in the requester's zone, whatever the host has installed

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/utils"
	"gorm.io/gorm"
)

const (
	// RowsPerFile caps each CSV so it opens in Excel (limit 1,048,576 rows).
	// Larger exports are split into several CSVs inside a zip.
	RowsPerFile = 100_000
	// exportBatchSize is how many rows each query reads.
	exportBatchSize = 5000
	// csvTimeLayout matches how msg91 and spreadsheets show times.
	csvTimeLayout = "2006-01-02 15:04:05"
	// utf8BOM makes Excel read the file as UTF-8, so non-Latin text (Hindi,
	// Tamil, emoji) isn't garbled.
	utf8BOM = "\xEF\xBB\xBF"
)

// Variables rather than constants so tests can exercise splitting and
// heartbeats without 100,000 rows or 30-second waits.
var (
	rowsPerFile int64 = RowsPerFile
	// heartbeatInterval is how often Export reports progress.
	heartbeatInterval = 30 * time.Second
)

// ExportOptions configures one message-log export.
type ExportOptions struct {
	DB      *gorm.DB
	OrgID   uuid.UUID
	Filters Filters
	// Location is the zone the file's times are written in.
	Location *time.Location
	// MaskPhones applies the organization's phone-number masking.
	MaskPhones bool
	// StorageRoot is the storage directory; the file is written under
	// StorageRoot/RelDir.
	StorageRoot string
	RelDir      string
	// BaseName names the file on disk (the export ID); the download name is
	// built from the date range.
	BaseName string
	// Heartbeat, if set, is called about every heartbeatInterval while rows
	// are written.
	Heartbeat func()
}

// ExportResult describes the generated file.
type ExportResult struct {
	FilePath string // relative to StorageRoot
	FileName string // name to download it as
	RowCount int64
	FileSize int64
}

// exportRow is the scan target for an export batch.
type exportRow struct {
	ID                uuid.UUID
	CreatedAt         time.Time
	Direction         string
	Source            string
	Status            string
	ContactName       string
	ContactPhone      string
	WhatsAppAccount   string
	MessageType       string
	TemplateName      string
	CampaignID        string
	Content           string
	ErrorMessage      string
	SentAt            *time.Time
	DeliveredAt       *time.Time
	ReadAt            *time.Time
	PricingCategory   string
	WhatsAppMessageID string
	SentByName        string
}

const exportColumns = `m.id, m.created_at, m.direction, COALESCE(m.source, '') AS source, m.status,
	COALESCE(c.profile_name, '') AS contact_name, COALESCE(c.phone_number, '') AS contact_phone,
	m.whats_app_account AS whats_app_account, m.message_type, COALESCE(m.template_name, '') AS template_name,
	COALESCE(m.metadata->>'campaign_id', '') AS campaign_id, COALESCE(m.content, '') AS content,
	COALESCE(m.error_message, '') AS error_message, m.sent_at, m.delivered_at, m.read_at,
	COALESCE(m.pricing_category, '') AS pricing_category,
	COALESCE(m.whats_app_message_id, '') AS whats_app_message_id,
	COALESCE(u.full_name, '') AS sent_by_name`

// Export writes the messages matching opts.Filters, oldest first. Up to
// RowsPerFile rows produce a single CSV; more produce a zip of CSVs, each
// named after the dates it covers. Nothing is left on disk if it fails.
func Export(ctx context.Context, opts ExportOptions) (ExportResult, error) {
	var contactIDs []uuid.UUID
	if opts.Filters.Phone != "" {
		var err error
		if contactIDs, err = ResolveContacts(opts.DB, opts.OrgID, opts.Filters.Phone); err != nil {
			return ExportResult{}, fmt.Errorf("resolve phone filter: %w", err)
		}
	}

	var total int64
	if err := opts.Filters.Apply(opts.DB.WithContext(ctx).Table("messages m"), opts.OrgID, contactIDs).
		Count(&total).Error; err != nil {
		return ExportResult{}, fmt.Errorf("count messages: %w", err)
	}

	dir := filepath.Join(opts.StorageRoot, opts.RelDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ExportResult{}, fmt.Errorf("create export directory: %w", err)
	}

	e := &exporter{opts: opts, contactIDs: contactIDs, campaignNames: map[string]string{}, lastBeat: time.Now()}
	rangeName := fmt.Sprintf("message-logs_%s_to_%s", opts.Filters.From.In(opts.Location).Format("2006-01-02"),
		opts.Filters.To.In(opts.Location).Format("2006-01-02"))

	var result ExportResult
	var err error
	if total <= rowsPerFile {
		result, err = e.writeSingleCSV(ctx, dir, rangeName)
	} else {
		result, err = e.writeZip(ctx, dir, rangeName)
	}
	if err != nil {
		return ExportResult{}, err
	}

	info, err := os.Stat(filepath.Join(opts.StorageRoot, result.FilePath))
	if err != nil {
		return ExportResult{}, fmt.Errorf("stat export file: %w", err)
	}
	result.FileSize = info.Size()
	return result, nil
}

type exporter struct {
	opts          ExportOptions
	contactIDs    []uuid.UUID
	campaignNames map[string]string
	lastBeat      time.Time
	// cursor is the last row written; batches continue after it.
	cursor *exportRow
}

func (e *exporter) writeSingleCSV(ctx context.Context, dir, rangeName string) (ExportResult, error) {
	finalPath := filepath.Join(dir, e.opts.BaseName+".csv")
	tmpPath := finalPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return ExportResult{}, fmt.Errorf("create export file: %w", err)
	}
	defer os.Remove(tmpPath) // no-op once renamed

	rows, _, _, err := e.writeCSV(ctx, f, 0)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return ExportResult{}, err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return ExportResult{}, fmt.Errorf("finalize export file: %w", err)
	}
	return ExportResult{
		FilePath: filepath.Join(e.opts.RelDir, e.opts.BaseName+".csv"),
		FileName: rangeName + ".csv",
		RowCount: rows,
	}, nil
}

func (e *exporter) writeZip(ctx context.Context, dir, rangeName string) (ExportResult, error) {
	finalPath := filepath.Join(dir, e.opts.BaseName+".zip")
	tmpPath := finalPath + ".tmp"
	zf, err := os.Create(tmpPath)
	if err != nil {
		return ExportResult{}, fmt.Errorf("create export file: %w", err)
	}
	defer os.Remove(tmpPath) // no-op once renamed

	zw := zip.NewWriter(zf)
	total, err := e.writeZipParts(ctx, zw, dir)
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := zf.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return ExportResult{}, err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return ExportResult{}, fmt.Errorf("finalize export file: %w", err)
	}
	return ExportResult{
		FilePath: filepath.Join(e.opts.RelDir, e.opts.BaseName+".zip"),
		FileName: rangeName + ".zip",
		RowCount: total,
	}, nil
}

// writeZipParts writes rowsPerFile-row CSVs into zw until the rows run out.
// Each part is written to a temporary file first because its name includes
// the dates it covers, which are only known once it is full.
func (e *exporter) writeZipParts(ctx context.Context, zw *zip.Writer, dir string) (int64, error) {
	var total int64
	for part := 1; ; part++ {
		partPath := filepath.Join(dir, fmt.Sprintf("%s.part%d.tmp", e.opts.BaseName, part))
		pf, err := os.Create(partPath)
		if err != nil {
			return total, fmt.Errorf("create export part: %w", err)
		}
		rows, first, last, err := e.writeCSV(ctx, pf, rowsPerFile)
		if closeErr := pf.Close(); err == nil {
			err = closeErr
		}
		if err == nil && rows > 0 {
			name := fmt.Sprintf("message-logs_part-%02d_%s_to_%s.csv", part,
				first.In(e.opts.Location).Format("2006-01-02"), last.In(e.opts.Location).Format("2006-01-02"))
			err = copyIntoZip(zw, name, partPath)
		}
		os.Remove(partPath)
		if err != nil {
			return total, err
		}
		total += rows
		if rows < rowsPerFile {
			return total, nil
		}
	}
}

func copyIntoZip(zw *zip.Writer, name, path string) error {
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open export part: %w", err)
	}
	defer src.Close()
	dst, err := zw.Create(name)
	if err != nil {
		return fmt.Errorf("add export part to zip: %w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("write export part to zip: %w", err)
	}
	return nil
}

// writeCSV writes the header and up to limit rows (0 = all remaining) to w,
// continuing after e.cursor. It returns the row count and the first and last
// rows' times.
func (e *exporter) writeCSV(ctx context.Context, w io.Writer, limit int64) (rows int64, first, last time.Time, err error) {
	if _, err := io.WriteString(w, utf8BOM); err != nil {
		return 0, first, last, err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(e.header()); err != nil {
		return 0, first, last, err
	}

	for limit == 0 || rows < limit {
		if err := ctx.Err(); err != nil {
			return rows, first, last, err
		}
		size := int64(exportBatchSize)
		if limit > 0 && limit-rows < size {
			size = limit - rows
		}
		batch, err := e.nextBatch(ctx, int(size))
		if err != nil {
			return rows, first, last, err
		}
		if err := e.loadCampaignNames(batch); err != nil {
			return rows, first, last, err
		}
		for i := range batch {
			if err := cw.Write(e.record(&batch[i])); err != nil {
				return rows, first, last, err
			}
		}
		if len(batch) > 0 {
			if rows == 0 {
				first = batch[0].CreatedAt
			}
			last = batch[len(batch)-1].CreatedAt
			e.cursor = &batch[len(batch)-1]
		}
		rows += int64(len(batch))
		e.beat()
		if int64(len(batch)) < size {
			break
		}
	}
	cw.Flush()
	return rows, first, last, cw.Error()
}

func (e *exporter) nextBatch(ctx context.Context, size int) ([]exportRow, error) {
	query := e.opts.Filters.Apply(e.opts.DB.WithContext(ctx).Table("messages m"), e.opts.OrgID, e.contactIDs).
		Select(exportColumns).
		Joins("LEFT JOIN contacts c ON c.id = m.contact_id").
		Joins("LEFT JOIN users u ON u.id = m.sent_by_user_id")
	if e.cursor != nil {
		query = query.Where("(m.created_at, m.id) > (?, ?)", e.cursor.CreatedAt, e.cursor.ID)
	}
	var batch []exportRow
	if err := query.Order("m.created_at ASC, m.id ASC").Limit(size).Scan(&batch).Error; err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	return batch, nil
}

// loadCampaignNames fetches names for campaigns not seen yet in this export.
func (e *exporter) loadCampaignNames(batch []exportRow) error {
	var ids []uuid.UUID
	for _, row := range batch {
		if _, seen := e.campaignNames[row.CampaignID]; seen || row.CampaignID == "" {
			continue
		}
		e.campaignNames[row.CampaignID] = "" // also covers IDs that don't parse or exist
		if id, err := uuid.Parse(row.CampaignID); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var campaigns []models.BulkMessageCampaign
	if err := e.opts.DB.Unscoped().Select("id", "name").
		Where("organization_id = ? AND id IN ?", e.opts.OrgID, ids).
		Find(&campaigns).Error; err != nil {
		return fmt.Errorf("load campaign names: %w", err)
	}
	for _, c := range campaigns {
		e.campaignNames[c.ID.String()] = c.Name
	}
	return nil
}

func (e *exporter) beat() {
	if e.opts.Heartbeat != nil && time.Since(e.lastBeat) >= heartbeatInterval {
		e.opts.Heartbeat()
		e.lastBeat = time.Now()
	}
}

func (e *exporter) header() []string {
	zone := e.opts.Location.String()
	return []string{
		"Time (" + zone + ")", "Direction", "Source", "Status", "Customer Name", "Customer Number",
		"WhatsApp Account", "Message Type", "Template", "Campaign", "Content", "Failure Reason",
		"Sent At (" + zone + ")", "Delivered At (" + zone + ")", "Read At (" + zone + ")",
		"Pricing Category", "Message ID", "WhatsApp Message ID", "Sent By",
	}
}

func (e *exporter) record(row *exportRow) []string {
	name, phone := row.ContactName, row.ContactPhone
	if e.opts.MaskPhones {
		name = utils.MaskIfPhoneNumber(name)
		phone = utils.MaskPhoneNumber(phone)
	}
	return []string{
		e.formatTime(&row.CreatedAt),
		row.Direction,
		row.Source,
		row.Status,
		csvSafe(name),
		csvSafe(phone),
		csvSafe(row.WhatsAppAccount),
		row.MessageType,
		csvSafe(row.TemplateName),
		csvSafe(e.campaignNames[row.CampaignID]),
		csvSafe(row.Content),
		csvSafe(row.ErrorMessage),
		e.formatTime(row.SentAt),
		e.formatTime(row.DeliveredAt),
		e.formatTime(row.ReadAt),
		row.PricingCategory,
		row.ID.String(),
		row.WhatsAppMessageID,
		csvSafe(row.SentByName),
	}
}

func (e *exporter) formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.In(e.opts.Location).Format(csvTimeLayout)
}

// csvSafe neutralises spreadsheet formula injection: a cell starting with
// =, +, -, @, tab or carriage return would be run as a formula by Excel or
// Sheets, so it gets a leading apostrophe, which they display as text.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// LoadLocation returns the named zone, or UTC for an empty name.
func LoadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, errors.New("unknown timezone " + name)
	}
	return loc, nil
}
