package api

// usage.go — the read side of the M2 ledger: paginated detail (/usage) and the
// three aggregate views (/stats/summary|trend|breakdown). Cost is settled here
// in Go, never stored: each aggregation cell carries (provider_id, model) so it
// can be priced via the three-layer rule (override ?? base×coefficient), then
// rolled up. A cell whose model is unknown contributes to cost_unknown_requests
// and leaves cost null rather than a misleading 0.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/pricing"
	"github.com/Code-kike/switchAPI/internal/hub/store"
)

// totals is the rolled-up shape shared by every stats view. Cost is nil until
// at least one priceable cell lands (null = unknown, not zero).
type totals struct {
	Requests            int64    `json:"requests"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheWriteTokens    int64    `json:"cache_write_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	Cost                *float64 `json:"cost"`
	CostUnknownRequests int64    `json:"cost_unknown_requests"`
}

// add folds one priced aggregation cell into the running totals.
func (t *totals) add(cell store.AggRow, cost float64, known bool) {
	t.Requests += cell.Requests
	t.InputTokens += cell.InputTokens
	t.OutputTokens += cell.OutputTokens
	t.CacheWriteTokens += cell.CacheWriteTokens
	t.CacheReadTokens += cell.CacheReadTokens
	if known {
		sum := cost
		if t.Cost != nil {
			sum += *t.Cost
		}
		t.Cost = &sum
	} else {
		t.CostUnknownRequests += cell.Requests
	}
}

// priceCell settles one aggregation cell through the three-layer rule.
func (s *Server) priceCell(coeffs map[string]float64, cell store.AggRow) (float64, bool) {
	if s.pricer == nil {
		return 0, false
	}
	u := pricing.Usage{
		Model: cell.Model, InputTokens: cell.InputTokens, OutputTokens: cell.OutputTokens,
		CacheWriteTokens: cell.CacheWriteTokens, CacheReadTokens: cell.CacheReadTokens,
	}
	var ovr *pricing.Prices
	if p, ok := s.pricer.Override(cell.ProviderID, cell.Model); ok {
		ovr = &p
	}
	coeff, ok := coeffs[cell.ProviderID]
	if !ok {
		coeff = 1.0 // provider gone but usage remains → assume base price
	}
	return s.pricer.Cost(u, coeff, ovr)
}

// coeffMap builds provider_id → cost_coefficient for the current request.
func (s *Server) coeffMap() (map[string]float64, error) {
	list, err := s.st.ListProviders()
	if err != nil {
		return nil, err
	}
	m := make(map[string]float64, len(list))
	for _, p := range list {
		m[p.ID] = p.CostCoefficient
	}
	return m, nil
}

// ---- detail ----

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	f := parseUsageFilter(r)
	q := r.URL.Query()
	f.Limit = clampInt(q.Get("limit"), 50, 500)
	f.Offset = atoiDefault(q.Get("offset"), 0)

	rows, total, err := s.st.QueryUsage(f)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	coeffs, err := s.coeffMap()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type usageRowView struct {
		store.UsageRow
		Cost *float64 `json:"cost"`
	}
	out := make([]usageRowView, 0, len(rows))
	for _, row := range rows {
		cell := store.AggRow{
			ProviderID: row.ProviderID, Model: effectiveModel(row.Model, row.ModelRedirected), Requests: 1,
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			CacheWriteTokens: row.CacheWriteTokens, CacheReadTokens: row.CacheReadTokens,
		}
		var costPtr *float64
		if cost, known := s.priceCell(coeffs, cell); known {
			costPtr = &cost
		}
		out = append(out, usageRowView{UsageRow: row, Cost: costPtr})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": out})
}

// effectiveModel is the model that actually ran upstream: the redirect target
// when set, else the requested model. Must match store.effModelExpr so detail
// and aggregate cost agree.
func effectiveModel(model, redirected string) string {
	if redirected != "" {
		return redirected
	}
	return model
}

// ---- summary ----

func (s *Server) handleStatsSummary(w http.ResponseWriter, r *http.Request) {
	f := parseUsageFilter(r)
	cells, err := s.st.AggSummary(f)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	coeffs, err := s.coeffMap()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type summaryResp struct {
		totals
		ByApp map[string]*totals `json:"by_app"`
	}
	resp := summaryResp{ByApp: map[string]*totals{}}
	for _, cell := range cells {
		cost, known := s.priceCell(coeffs, cell)
		resp.totals.add(cell, cost, known)
		per := resp.ByApp[cell.App]
		if per == nil {
			per = &totals{}
			resp.ByApp[cell.App] = per
		}
		per.add(cell, cost, known)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- trend ----

func (s *Server) handleStatsTrend(w http.ResponseWriter, r *http.Request) {
	f := parseUsageFilter(r)
	var span int64 = 3600
	if r.URL.Query().Get("bucket") == "day" {
		span = 86400
	}
	cells, err := s.st.AggTrend(f, span)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	coeffs, err := s.coeffMap()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type trendEntry struct {
		BucketTS int64 `json:"bucket_ts"`
		totals
	}
	byBucket := map[int64]*trendEntry{}
	var order []int64
	for _, cell := range cells {
		e := byBucket[cell.BucketTS]
		if e == nil {
			e = &trendEntry{BucketTS: cell.BucketTS}
			byBucket[cell.BucketTS] = e
			order = append(order, cell.BucketTS)
		}
		cost, known := s.priceCell(coeffs, cell)
		e.totals.add(cell, cost, known)
	}
	out := make([]*trendEntry, 0, len(order))
	for _, ts := range order { // AggTrend returns cells ordered by bucket
		out = append(out, byBucket[ts])
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- breakdown ----

var breakdownDim = map[string]string{
	"provider": "provider_id",
	"model":    "model",
	"app":      "app",
	"device":   "device_id",
}

func (s *Server) handleStatsBreakdown(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	dim, ok := breakdownDim[by]
	if !ok {
		httpError(w, http.StatusBadRequest, "by 必须是 provider|model|app|device")
		return
	}
	f := parseUsageFilter(r)
	cells, err := s.st.AggBreakdown(f, dim)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	coeffs, err := s.coeffMap()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	names := map[string]string{}
	if by == "provider" {
		if list, err := s.st.ListProviders(); err == nil {
			for _, p := range list {
				names[p.ID] = p.Name
			}
		}
	}

	type breakdownEntry struct {
		Key  string `json:"key"`
		Name string `json:"name,omitempty"`
		totals
	}
	byKey := map[string]*breakdownEntry{}
	var order []string
	for _, cell := range cells {
		e := byKey[cell.Key]
		if e == nil {
			e = &breakdownEntry{Key: cell.Key, Name: names[cell.Key]}
			byKey[cell.Key] = e
			order = append(order, cell.Key)
		}
		cost, known := s.priceCell(coeffs, cell)
		e.totals.add(cell, cost, known)
	}
	out := make([]*breakdownEntry, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- query-param helpers ----

// parseUsageFilter reads the shared filter params, defaulting to the last 7
// days when no time range is given.
func parseUsageFilter(r *http.Request) store.UsageFilter {
	q := r.URL.Query()
	f := store.UsageFilter{
		App:        q.Get("app"),
		ProviderID: q.Get("provider_id"),
		Model:      q.Get("model"),
		DeviceID:   q.Get("device_id"),
		From:       atoiDefault64(q.Get("from"), 0),
		To:         atoiDefault64(q.Get("to"), 0),
	}
	if f.From == 0 && f.To == 0 {
		now := time.Now().Unix()
		f.To = now
		f.From = now - 7*86400
	}
	return f
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func atoiDefault64(s string, def int64) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return def
}

// clampInt parses s, applies the default when empty/invalid, and caps at max.
func clampInt(s string, def, max int) int {
	n := atoiDefault(s, def)
	if n <= 0 {
		n = def
	}
	if n > max {
		n = max
	}
	return n
}
