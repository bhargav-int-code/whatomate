package handlers

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// Shared plumbing for the log pages (message logs, webhook logs): fixed-size
// pages walked with (created_at, id) cursors instead of counts, a required
// date range limited to the last 90 days, and a per-query statement timeout.

const (
	// logsPageSize is the fixed page size; there is no total count.
	logsPageSize = 50
	// logsMaxAge is how far back the logs can be filtered.
	logsMaxAge = 90 * 24 * time.Hour
	// logsStatementTimeout bounds every logs query so an unusual filter
	// combination fails fast instead of tying up the database.
	logsStatementTimeout = "5s"
	// pgQueryCanceled is Postgres' SQLSTATE for a statement_timeout cancel.
	pgQueryCanceled = "57014"
)

// logCursor identifies the row a page starts after. Rows are ordered by
// (created_at, id) so rows sharing a timestamp page deterministically.
type logCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func encodeLogCursor(createdAt time.Time, id uuid.UUID) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeLogCursor(s string) (*logCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil, errors.New("malformed cursor")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, err
	}
	rowID, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	return &logCursor{CreatedAt: createdAt, ID: rowID}, nil
}

// parseLogTime accepts a YYYY-MM-DD date (UTC) or an RFC3339 timestamp, so the
// UI can send exact local-day boundaries. endOfRange extends a bare date to the
// end of that day.
func parseLogTime(s string, endOfRange bool) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		if endOfRange {
			return endOfDay(t), true
		}
		return t, true
	}
	return time.Time{}, false
}

// parseLogRange reads the required from/to query params. On failure it returns
// a message suitable for a 400 response.
func parseLogRange(args *fasthttp.Args) (from, to time.Time, errMsg string) {
	from, okFrom := parseLogTime(string(args.Peek("from")), false)
	to, okTo := parseLogTime(string(args.Peek("to")), true)
	if !okFrom || !okTo {
		return from, to, "from and to are required (YYYY-MM-DD or RFC3339)"
	}
	if to.Before(from) {
		return from, to, "from must be before to"
	}
	// Allow a day of slack so a "last 90 days" range in any timezone passes.
	if from.Before(time.Now().Add(-logsMaxAge - 24*time.Hour)) {
		return from, to, "Logs are available for the last 90 days only"
	}
	return from, to, ""
}

// logPage describes how to query one page: from the newest rows (no cursor),
// or older/newer than a cursor.
type logPage struct {
	cursor *logCursor
	older  bool
}

// parseLogPage reads the cursor and dir query params.
func parseLogPage(args *fasthttp.Args) (logPage, error) {
	page := logPage{older: string(args.Peek("dir")) != "newer"}
	if c := string(args.Peek("cursor")); c != "" {
		cursor, err := decodeLogCursor(c)
		if err != nil {
			return page, err
		}
		page.cursor = cursor
	}
	return page, nil
}

// apply adds the cursor condition and ordering for table alias t, and fetches
// one extra row so the caller can tell whether another page exists.
func (p logPage) apply(query *gorm.DB, t string) *gorm.DB {
	switch {
	case p.cursor == nil:
		query = query.Order(t + ".created_at DESC, " + t + ".id DESC")
	case p.older:
		query = query.Where("("+t+".created_at, "+t+".id) < (?, ?)", p.cursor.CreatedAt, p.cursor.ID).
			Order(t + ".created_at DESC, " + t + ".id DESC")
	default:
		query = query.Where("("+t+".created_at, "+t+".id) > (?, ?)", p.cursor.CreatedAt, p.cursor.ID).
			Order(t + ".created_at ASC, " + t + ".id ASC")
	}
	return query.Limit(logsPageSize + 1)
}

// finishLogPage trims the extra row, returns rows newest-first and works out
// the cursors for the neighbouring pages. key returns a row's (created_at, id).
func finishLogPage[T any](p logPage, rows []T, key func(T) (time.Time, uuid.UUID)) (page []T, nextCursor, prevCursor string) {
	hasMore := len(rows) > logsPageSize
	if hasMore {
		rows = rows[:logsPageSize]
	}
	if !p.older {
		// Newer pages are fetched oldest-first; always return newest-first.
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	if len(rows) == 0 {
		return rows, "", ""
	}

	first, last := rows[0], rows[len(rows)-1]
	encode := func(row T) string { return encodeLogCursor(key(row)) }
	if p.older {
		if hasMore {
			nextCursor = encode(last)
		}
		if p.cursor != nil {
			prevCursor = encode(first)
		}
	} else {
		// We came from an older page, so older rows exist.
		nextCursor = encode(last)
		if hasMore {
			prevCursor = encode(first)
		}
	}
	return rows, nextCursor, prevCursor
}

// withStatementTimeout runs fn in a transaction whose statements are cancelled
// by Postgres after logsStatementTimeout.
func (a *App) withStatementTimeout(fn func(tx *gorm.DB) error) error {
	return a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL statement_timeout = '" + logsStatementTimeout + "'").Error; err != nil {
			return err
		}
		return fn(tx)
	})
}

// sendLogQueryError maps a timed-out query to a 422 asking the caller to
// narrow the filters, and anything else to a 500. label names the logs, e.g.
// "message logs".
func (a *App) sendLogQueryError(r *fastglue.Request, err error, label string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgQueryCanceled {
		return r.SendErrorEnvelope(fasthttp.StatusUnprocessableEntity,
			"Query took too long. Narrow the date range or add filters.", nil, "")
	}
	a.Log.Error("Failed to load "+label, "error", err)
	return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load "+label, nil, "")
}

func logsEnvelope(key string, logs any, nextCursor, prevCursor string) map[string]any {
	return map[string]any{
		key:           logs,
		"next_cursor": nextCursor,
		"prev_cursor": prevCursor,
		"limit":       logsPageSize,
	}
}
