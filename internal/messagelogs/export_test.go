package messagelogs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var ist = func() *time.Location { l, _ := time.LoadLocation("Asia/Kolkata"); return l }()

type exportFixture struct {
	db       *gorm.DB
	org      *models.Organization
	user     *models.User
	contact  *models.Contact
	campaign models.BulkMessageCampaign
	root     string
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithFullName("Priya Agent"))
	account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
	contact := testutil.CreateTestContactWith(t, db, org.ID,
		testutil.WithContactAccount(account.Name),
		testutil.WithPhoneNumber(fmt.Sprintf("9198%08d", rand.Intn(100_000_000))))
	tmpl := testutil.CreateTestTemplate(t, db, org.ID, account.Name)
	campaign := models.BulkMessageCampaign{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, WhatsAppAccount: account.Name,
		Name: "Weekend Offer", TemplateID: tmpl.ID, CreatedBy: user.ID,
	}
	require.NoError(t, db.Create(&campaign).Error)
	return &exportFixture{db: db, org: org, user: user, contact: contact, campaign: campaign, root: t.TempDir()}
}

func (f *exportFixture) message(t *testing.T, createdAt time.Time, mutate func(*models.Message)) models.Message {
	t.Helper()
	m := models.Message{
		BaseModel:       models.BaseModel{ID: uuid.New(), CreatedAt: createdAt},
		OrganizationID:  f.org.ID,
		WhatsAppAccount: "Main",
		ContactID:       f.contact.ID,
		Direction:       models.DirectionOutgoing,
		MessageType:     models.MessageTypeText,
		Content:         "hello",
		Status:          models.MessageStatusSent,
		Source:          models.MessageSourceAPI,
	}
	if mutate != nil {
		mutate(&m)
	}
	require.NoError(t, f.db.Create(&m).Error)
	return m
}

func (f *exportFixture) export(t *testing.T, filters Filters, mutate func(*ExportOptions)) (ExportResult, error) {
	t.Helper()
	opts := ExportOptions{
		DB: f.db, OrgID: f.org.ID, Filters: filters, Location: ist,
		StorageRoot: f.root, RelDir: filepath.Join("exports", f.org.ID.String()), BaseName: uuid.NewString(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	return Export(context.Background(), opts)
}

func lastDay() Filters {
	return Filters{From: time.Now().Add(-24 * time.Hour), To: time.Now().Add(time.Minute)}
}

// readCSV parses CSV bytes after checking and stripping the UTF-8 BOM.
func readCSV(t *testing.T, data []byte) [][]string {
	t.Helper()
	require.True(t, bytes.HasPrefix(data, []byte(utf8BOM)), "CSV starts with a UTF-8 BOM for Excel")
	records, err := csv.NewReader(bytes.NewReader(data[len(utf8BOM):])).ReadAll()
	require.NoError(t, err)
	return records
}

func TestExport_SingleCSVContents(t *testing.T) {
	f := newExportFixture(t)
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	older := f.message(t, base, func(m *models.Message) {
		m.MessageType = models.MessageTypeTemplate
		m.TemplateName = "booking_confirmation_v8"
		m.Content = "நீங்கள் turf நடத்துகிறீர்களா?"
		m.Metadata = models.JSONB{"campaign_id": f.campaign.ID.String()}
		m.Source = models.MessageSourceCampaign
		delivered := base.Add(5 * time.Second)
		m.DeliveredAt = &delivered
		m.PricingCategory = "utility"
	})
	f.message(t, base.Add(time.Minute), func(m *models.Message) {
		m.Content = "=HYPERLINK(\"http://evil\")"
		m.SentByUserID = &f.user.ID
		m.Status = models.MessageStatusFailed
		m.ErrorMessage = "131026: Message undeliverable"
	})

	res, err := f.export(t, lastDay(), nil)
	require.NoError(t, err)
	assert.EqualValues(t, 2, res.RowCount)
	assert.True(t, strings.HasSuffix(res.FileName, ".csv"))
	assert.True(t, strings.HasPrefix(res.FileName, "message-logs_"))

	data, err := os.ReadFile(filepath.Join(f.root, res.FilePath))
	require.NoError(t, err)
	assert.EqualValues(t, len(data), res.FileSize)
	records := readCSV(t, data)
	require.Len(t, records, 3)

	header := records[0]
	assert.Equal(t, "Time (Asia/Kolkata)", header[0])
	assert.Equal(t, "Delivered At (Asia/Kolkata)", header[13])

	first := records[1] // oldest first
	assert.Equal(t, base.In(ist).Format(csvTimeLayout), first[0], "times are in the requester's zone")
	assert.Equal(t, "campaign", first[2])
	assert.Equal(t, f.contact.PhoneNumber, first[5])
	assert.Equal(t, "booking_confirmation_v8", first[8])
	assert.Equal(t, "Weekend Offer", first[9])
	assert.Equal(t, "நீங்கள் turf நடத்துகிறீர்களா?", first[10], "non-Latin text survives")
	assert.Equal(t, base.Add(5*time.Second).In(ist).Format(csvTimeLayout), first[13])
	assert.Equal(t, "", first[14], "no read time")
	assert.Equal(t, "utility", first[15])
	assert.Equal(t, older.ID.String(), first[16])

	second := records[2]
	assert.Equal(t, `'=HYPERLINK("http://evil")`, second[10], "formulas are neutralised")
	assert.Equal(t, "131026: Message undeliverable", second[11])
	assert.Equal(t, "Priya Agent", second[18])
}

func TestExport_AppliesFiltersAndMasking(t *testing.T) {
	f := newExportFixture(t)
	now := time.Now().Add(-time.Hour)
	f.message(t, now, nil)
	failed := f.message(t, now.Add(time.Second), func(m *models.Message) { m.Status = models.MessageStatusFailed })

	filters := lastDay()
	filters.Status = "failed"
	res, err := f.export(t, filters, func(o *ExportOptions) { o.MaskPhones = true })
	require.NoError(t, err)
	assert.EqualValues(t, 1, res.RowCount)

	data, err := os.ReadFile(filepath.Join(f.root, res.FilePath))
	require.NoError(t, err)
	records := readCSV(t, data)
	require.Len(t, records, 2)
	assert.Equal(t, failed.ID.String(), records[1][16])
	assert.NotEqual(t, f.contact.PhoneNumber, records[1][5])
	assert.Contains(t, records[1][5], "*", "phone masked")

	// Phone filter that matches nobody exports just the header.
	filters = lastDay()
	filters.Phone = "10000000000"
	res, err = f.export(t, filters, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 0, res.RowCount)
}

func TestExport_SplitsIntoZip(t *testing.T) {
	f := newExportFixture(t)
	defer func(n int64) { rowsPerFile = n }(rowsPerFile)
	rowsPerFile = 3

	base := time.Now().Add(-3 * time.Hour)
	var ids []string
	for i := 0; i < 7; i++ {
		ids = append(ids, f.message(t, base.Add(time.Duration(i)*time.Minute), nil).ID.String())
	}

	res, err := f.export(t, lastDay(), nil)
	require.NoError(t, err)
	assert.EqualValues(t, 7, res.RowCount)
	assert.True(t, strings.HasSuffix(res.FileName, ".zip"))

	zr, err := zip.OpenReader(filepath.Join(f.root, res.FilePath))
	require.NoError(t, err)
	defer zr.Close()
	require.Len(t, zr.File, 3, "3 + 3 + 1 rows")

	var got []string
	for i, zf := range zr.File {
		assert.True(t, strings.HasPrefix(zf.Name, fmt.Sprintf("message-logs_part-%02d_", i+1)), zf.Name)
		rc, err := zf.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		rc.Close()
		require.NoError(t, err)
		records := readCSV(t, data)
		assert.Equal(t, "Time (Asia/Kolkata)", records[0][0], "every part has the header")
		for _, r := range records[1:] {
			got = append(got, r[16])
		}
	}
	assert.Equal(t, ids, got, "parts cover every row once, oldest first")

	// Only the zip remains: part temp files are cleaned up.
	entries, err := os.ReadDir(filepath.Dir(filepath.Join(f.root, res.FilePath)))
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestExport_HeartbeatsWhileWriting(t *testing.T) {
	f := newExportFixture(t)
	defer func(d time.Duration) { heartbeatInterval = d }(heartbeatInterval)
	heartbeatInterval = 0
	f.message(t, time.Now().Add(-time.Hour), nil)

	beats := 0
	_, err := f.export(t, lastDay(), func(o *ExportOptions) { o.Heartbeat = func() { beats++ } })
	require.NoError(t, err)
	assert.Positive(t, beats)
}

func TestExport_CancelledLeavesNothingBehind(t *testing.T) {
	f := newExportFixture(t)
	f.message(t, time.Now().Add(-time.Hour), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Export(ctx, ExportOptions{
		DB: f.db, OrgID: f.org.ID, Filters: lastDay(), Location: ist,
		StorageRoot: f.root, RelDir: "exports", BaseName: "x",
	})
	require.Error(t, err)

	entries, _ := os.ReadDir(filepath.Join(f.root, "exports"))
	assert.Empty(t, entries)
}

func TestFiltersJSONBRoundTrip(t *testing.T) {
	in := Filters{From: time.Now().Add(-time.Hour).UTC(), To: time.Now().UTC(), Status: "failed", Phone: "919800000000", Template: "otp"}
	out, err := FiltersFromJSONB(in.JSONB())
	require.NoError(t, err)
	assert.True(t, in.From.Equal(out.From))
	assert.True(t, in.To.Equal(out.To))
	assert.Equal(t, in.Status, out.Status)
	assert.Equal(t, in.Phone, out.Phone)
	assert.Equal(t, in.Template, out.Template)

	bad := in.JSONB()
	bad["status"] = "bounced"
	_, err = FiltersFromJSONB(bad)
	assert.ErrorIs(t, err, ErrInvalidFilter)
}

func TestCSVSafeAndNormalizePhone(t *testing.T) {
	for _, s := range []string{"=1+1", "+91", "-x", "@SUM", "\tx", "\rx"} {
		assert.Equal(t, "'"+s, csvSafe(s))
	}
	assert.Equal(t, "hello", csvSafe("hello"))
	assert.Equal(t, "", csvSafe(""))
	assert.Equal(t, "919876543210", NormalizePhone("+91 98765-43210"))
	assert.Equal(t, "", NormalizePhone("abc"))
}
