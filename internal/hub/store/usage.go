package store

// usage.go — the M2 usage ledger: idempotent batch ingest (INSERT OR IGNORE on
// the request_id UNIQUE key), filtered detail queries, and the aggregation
// helpers behind /stats. Every aggregate is ALSO grouped by (provider_id,
// model) so the API layer can price each cell in Go — cost is never stored.

import (
	"strconv"
	"strings"

	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

// UsageRow mirrors one usage_records row (detail query result).
type UsageRow struct {
	ID               int64  `json:"id"`
	TS               int64  `json:"ts"`
	DeviceID         string `json:"device_id"`
	App              string `json:"app"`
	ProviderID       string `json:"provider_id"`
	Model            string `json:"model"`
	ModelRedirected  string `json:"model_redirected"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	DurationMS       int64  `json:"duration_ms"`
	Status           int    `json:"status"`
	ErrorKind        string `json:"error_kind"`
	UsageSource      string `json:"usage_source"`
	RequestID        string `json:"request_id"`
}

// UsageFilter narrows both detail queries and aggregations. Zero-value fields
// are ignored. From/To are unix seconds (inclusive).
type UsageFilter struct {
	From, To   int64
	App        string
	ProviderID string
	Model      string
	DeviceID   string
	Limit      int
	Offset     int
}

// AggRow is one aggregation cell. It always carries ProviderID+Model (the
// pricing key); BucketTS is set by AggTrend, Key/App by AggBreakdown/AggSummary.
type AggRow struct {
	BucketTS         int64
	Key              string
	App              string
	ProviderID       string
	Model            string
	Requests         int64
	InputTokens      int64
	OutputTokens     int64
	CacheWriteTokens int64
	CacheReadTokens  int64
}

// InsertUsageRecords ingests a batch in a single transaction. device_id is
// attributed by the Hub from the reporting connection, never trusted from the
// Agent. INSERT OR IGNORE makes at-least-once delivery idempotent on
// request_id; the returned count is how many rows were actually new.
func (s *Store) InsertUsageRecords(deviceID string, recs []wire.UsageRecord) (int, error) {
	if len(recs) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO usage_records
		(ts, device_id, app, provider_id, model, model_redirected,
		 input_tokens, output_tokens, cache_write_tokens, cache_read_tokens,
		 duration_ms, status, error_kind, usage_source, request_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	inserted := 0
	for _, r := range recs {
		res, err := stmt.Exec(r.TS, deviceID, r.App, r.ProviderID, r.Model, r.ModelRedirected,
			r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens,
			r.DurationMS, r.Status, r.ErrorKind, usageSourceOrDefault(r.UsageSource), r.RequestID)
		if err != nil {
			return inserted, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return inserted, err
	}
	return inserted, nil
}

// usageSourceOrDefault guards the CHECK constraint: an empty string from an
// older Agent would violate the enum, so treat it as wire.
func usageSourceOrDefault(src string) string {
	switch src {
	case "wire", "estimated", "none":
		return src
	default:
		return "wire"
	}
}

const usageRowCols = `id, ts, device_id, app, provider_id, model, model_redirected,
	input_tokens, output_tokens, cache_write_tokens, cache_read_tokens,
	duration_ms, status, error_kind, usage_source, request_id`

// where builds the shared WHERE clause and args for detail and aggregate
// queries. It returns a leading " WHERE ..." (or "" when unfiltered).
func (f UsageFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.From > 0 {
		conds = append(conds, "ts >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		conds = append(conds, "ts <= ?")
		args = append(args, f.To)
	}
	if f.App != "" {
		conds = append(conds, "app = ?")
		args = append(args, f.App)
	}
	if f.ProviderID != "" {
		conds = append(conds, "provider_id = ?")
		args = append(args, f.ProviderID)
	}
	if f.Model != "" {
		// 按实际执行的模型匹配（与聚合/计价口径一致）：重定向请求可用
		// 重定向目标名筛出，原始请求名不再命中。
		conds = append(conds, "("+effModelExpr+") = ?")
		args = append(args, f.Model)
	}
	if f.DeviceID != "" {
		conds = append(conds, "device_id = ?")
		args = append(args, f.DeviceID)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// QueryUsage returns a page of detail rows (newest first) plus the total count
// matching the filter (ignoring Limit/Offset).
func (s *Store) QueryUsage(f UsageFilter) ([]UsageRow, int, error) {
	whereSQL, args := f.where()

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_records`+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + usageRowCols + ` FROM usage_records` + whereSQL +
		` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(q, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.ID, &r.TS, &r.DeviceID, &r.App, &r.ProviderID, &r.Model,
			&r.ModelRedirected, &r.InputTokens, &r.OutputTokens, &r.CacheWriteTokens,
			&r.CacheReadTokens, &r.DurationMS, &r.Status, &r.ErrorKind, &r.UsageSource,
			&r.RequestID); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

const aggSums = `SUM(input_tokens), SUM(output_tokens), SUM(cache_write_tokens),
	SUM(cache_read_tokens), COUNT(*)`

// effModelExpr is the model that actually ran upstream: the redirect target
// when a model redirect applied, else the requested model. Pricing and the
// by-model breakdown key on this, so a redirected request (retired requested
// name → live target) prices against the model that truly ran, not the
// original that would fall to unknown.
const effModelExpr = `CASE WHEN model_redirected <> '' THEN model_redirected ELSE model END`

// AggSummary groups by (app, provider_id, effective-model). The API layer
// prices each cell then rolls up grand totals and the per-app split.
func (s *Store) AggSummary(f UsageFilter) ([]AggRow, error) {
	whereSQL, args := f.where()
	q := `SELECT app, provider_id, ` + effModelExpr + ` AS eff_model, ` + aggSums + `
		FROM usage_records` + whereSQL + `
		GROUP BY app, provider_id, eff_model`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AggRow
	for rows.Next() {
		var a AggRow
		if err := rows.Scan(&a.App, &a.ProviderID, &a.Model, &a.InputTokens, &a.OutputTokens,
			&a.CacheWriteTokens, &a.CacheReadTokens, &a.Requests); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AggTrend groups by (time bucket, provider_id, effective-model). bucketSpan is
// 3600 (hour) or 86400 (day); the bucket start is integer-floored ts/span*span.
func (s *Store) AggTrend(f UsageFilter, bucketSpan int64) ([]AggRow, error) {
	if bucketSpan <= 0 {
		bucketSpan = 3600
	}
	whereSQL, args := f.where()
	// bucketSpan is a validated int constant (3600/86400), never user text.
	span := strconv.FormatInt(bucketSpan, 10)
	q := `SELECT (ts/` + span + `)*` + span + ` AS bucket,
		provider_id, ` + effModelExpr + ` AS eff_model, ` + aggSums + `
		FROM usage_records` + whereSQL + `
		GROUP BY bucket, provider_id, eff_model
		ORDER BY bucket`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AggRow
	for rows.Next() {
		var a AggRow
		if err := rows.Scan(&a.BucketTS, &a.ProviderID, &a.Model, &a.InputTokens, &a.OutputTokens,
			&a.CacheWriteTokens, &a.CacheReadTokens, &a.Requests); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AggBreakdown groups by (dimension, provider_id, effective-model). dim is one
// of provider_id | model | app | device_id (validated by the caller); when dim
// is "model" the key is the effective model so it lines up with pricing.
func (s *Store) AggBreakdown(f UsageFilter, dim string) ([]AggRow, error) {
	whereSQL, args := f.where()
	// dim is validated against a fixed whitelist by the API layer.
	keyExpr := dim
	if dim == "model" {
		keyExpr = effModelExpr
	}
	q := `SELECT ` + keyExpr + ` AS key, provider_id, ` + effModelExpr + ` AS eff_model, ` + aggSums + `
		FROM usage_records` + whereSQL + `
		GROUP BY key, provider_id, eff_model`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AggRow
	for rows.Next() {
		var a AggRow
		if err := rows.Scan(&a.Key, &a.ProviderID, &a.Model, &a.InputTokens, &a.OutputTokens,
			&a.CacheWriteTokens, &a.CacheReadTokens, &a.Requests); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
