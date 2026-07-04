package store

import (
	"testing"

	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

func rec(reqID, app, provider, model string, ts int64, in, out, cw, cr int64) wire.UsageRecord {
	return wire.UsageRecord{
		RequestID: reqID, TS: ts, App: app, ProviderID: provider, Model: model,
		InputTokens: in, OutputTokens: out, CacheWriteTokens: cw, CacheReadTokens: cr,
		DurationMS: 100, Status: 200, UsageSource: "wire",
	}
}

func TestInsertUsageRecordsIdempotent(t *testing.T) {
	s := openTest(t)
	batch := []wire.UsageRecord{
		rec("r1", "claude-code", "p1", "claude-haiku-4-5", 1000, 10, 20, 0, 0),
		rec("r2", "claude-code", "p1", "claude-haiku-4-5", 1001, 5, 6, 0, 0),
	}
	n, err := s.InsertUsageRecords("d1", batch)
	if err != nil || n != 2 {
		t.Fatalf("first insert = %d err=%v, want 2", n, err)
	}
	// Same batch again → all ignored (request_id UNIQUE dedup).
	n, err = s.InsertUsageRecords("d1", batch)
	if err != nil || n != 0 {
		t.Fatalf("re-insert = %d err=%v, want 0", n, err)
	}
	// Mixed batch: one new, one dup.
	n, err = s.InsertUsageRecords("d1", []wire.UsageRecord{
		batch[0],
		rec("r3", "codex", "p2", "gpt-5.1-codex", 1002, 7, 8, 0, 3),
	})
	if err != nil || n != 1 {
		t.Fatalf("mixed insert = %d err=%v, want 1", n, err)
	}

	rows, total, err := s.QueryUsage(UsageFilter{})
	if err != nil || total != 3 || len(rows) != 3 {
		t.Fatalf("query all: total=%d rows=%d err=%v", total, len(rows), err)
	}
	// device_id attributed by the Hub, not the record.
	if rows[0].DeviceID != "d1" {
		t.Fatalf("device_id = %q", rows[0].DeviceID)
	}
}

func TestUsageSourceDefaulted(t *testing.T) {
	s := openTest(t)
	r := rec("r1", "codex", "p1", "m", 1000, 1, 1, 0, 0)
	r.UsageSource = "" // older agent → must default to wire, not violate CHECK
	if _, err := s.InsertUsageRecords("d1", []wire.UsageRecord{r}); err != nil {
		t.Fatalf("insert empty source: %v", err)
	}
	rows, _, _ := s.QueryUsage(UsageFilter{})
	if rows[0].UsageSource != "wire" {
		t.Fatalf("usage_source = %q, want wire", rows[0].UsageSource)
	}
}

func TestQueryUsageFiltersAndPaging(t *testing.T) {
	s := openTest(t)
	recs := []wire.UsageRecord{
		rec("r1", "claude-code", "p1", "m1", 1000, 1, 1, 0, 0),
		rec("r2", "codex", "p2", "m2", 2000, 1, 1, 0, 0),
		rec("r3", "claude-code", "p1", "m1", 3000, 1, 1, 0, 0),
		rec("r4", "claude-code", "p3", "m3", 4000, 1, 1, 0, 0),
	}
	if _, err := s.InsertUsageRecords("dA", recs[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertUsageRecords("dB", recs[3:]); err != nil {
		t.Fatal(err)
	}

	// App filter.
	_, total, _ := s.QueryUsage(UsageFilter{App: "claude-code"})
	if total != 3 {
		t.Fatalf("app filter total = %d, want 3", total)
	}
	// Provider + device filter.
	_, total, _ = s.QueryUsage(UsageFilter{ProviderID: "p1"})
	if total != 2 {
		t.Fatalf("provider filter total = %d, want 2", total)
	}
	_, total, _ = s.QueryUsage(UsageFilter{DeviceID: "dB"})
	if total != 1 {
		t.Fatalf("device filter total = %d, want 1", total)
	}
	// Time range [2000, 3000].
	_, total, _ = s.QueryUsage(UsageFilter{From: 2000, To: 3000})
	if total != 2 {
		t.Fatalf("time filter total = %d, want 2", total)
	}
	// Newest-first ordering.
	rows, _, _ := s.QueryUsage(UsageFilter{})
	if rows[0].TS != 4000 || rows[3].TS != 1000 {
		t.Fatalf("ordering wrong: %d..%d", rows[0].TS, rows[3].TS)
	}
	// Paging.
	rows, total, _ = s.QueryUsage(UsageFilter{Limit: 2, Offset: 0})
	if total != 4 || len(rows) != 2 || rows[0].TS != 4000 {
		t.Fatalf("page1: total=%d len=%d", total, len(rows))
	}
	rows, _, _ = s.QueryUsage(UsageFilter{Limit: 2, Offset: 2})
	if len(rows) != 2 || rows[0].TS != 2000 {
		t.Fatalf("page2: len=%d first=%d", len(rows), rows[0].TS)
	}
}

func TestAggregations(t *testing.T) {
	s := openTest(t)
	// Two apps, two providers/models, spread across two hours.
	recs := []wire.UsageRecord{
		rec("r1", "claude-code", "p1", "m1", 3600, 10, 1, 0, 0),
		rec("r2", "claude-code", "p1", "m1", 3700, 20, 2, 0, 0), // same hour bucket as r1
		rec("r3", "codex", "p2", "m2", 7200, 5, 5, 0, 3),        // next hour bucket
	}
	if _, err := s.InsertUsageRecords("d1", recs); err != nil {
		t.Fatal(err)
	}

	// Summary: grouped by (app, provider, model).
	sum, err := s.AggSummary(UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var ccInput, codexReq int64
	for _, a := range sum {
		if a.App == "claude-code" {
			ccInput += a.InputTokens
		}
		if a.App == "codex" {
			codexReq += a.Requests
		}
	}
	if ccInput != 30 || codexReq != 1 {
		t.Fatalf("summary: ccInput=%d codexReq=%d", ccInput, codexReq)
	}

	// Trend by hour: two buckets.
	tr, err := s.AggTrend(UsageFilter{}, 3600)
	if err != nil {
		t.Fatal(err)
	}
	buckets := map[int64]int64{}
	for _, a := range tr {
		buckets[a.BucketTS] += a.Requests
	}
	if buckets[3600] != 2 || buckets[7200] != 1 {
		t.Fatalf("hour buckets = %v", buckets)
	}
	// Trend by day: single bucket at 0.
	tr, _ = s.AggTrend(UsageFilter{}, 86400)
	dayBuckets := map[int64]int64{}
	for _, a := range tr {
		dayBuckets[a.BucketTS] += a.Requests
	}
	if len(dayBuckets) != 1 || dayBuckets[0] != 3 {
		t.Fatalf("day buckets = %v", dayBuckets)
	}

	// Breakdown by provider: p1 has 2 requests, p2 has 1.
	bd, err := s.AggBreakdown(UsageFilter{}, "provider_id")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]int64{}
	for _, a := range bd {
		byKey[a.Key] += a.Requests
		if a.ProviderID == "" || a.Model == "" {
			t.Fatal("breakdown cell missing pricing key")
		}
	}
	if byKey["p1"] != 2 || byKey["p2"] != 1 {
		t.Fatalf("breakdown provider = %v", byKey)
	}
}

func TestAggregationsUseEffectiveModel(t *testing.T) {
	s := openTest(t)
	// A redirected request: requested a retired name, ran the live target.
	r1 := rec("r1", "claude-code", "p1", "claude-3-5-haiku-20241022", 1000, 10, 1, 0, 0)
	r1.ModelRedirected = "claude-haiku-4-5"
	// A plain request on the same live model, no redirect.
	r2 := rec("r2", "claude-code", "p1", "claude-haiku-4-5", 1100, 20, 2, 0, 0)
	if _, err := s.InsertUsageRecords("d1", []wire.UsageRecord{r1, r2}); err != nil {
		t.Fatal(err)
	}

	// Summary keys on the effective model → both records collapse to one cell.
	sum, err := s.AggSummary(UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum) != 1 || sum[0].Model != "claude-haiku-4-5" || sum[0].Requests != 2 {
		t.Fatalf("summary effective-model grouping wrong: %+v", sum)
	}

	// Breakdown by=model keys on the effective model too.
	bd, err := s.AggBreakdown(UsageFilter{}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if len(bd) != 1 || bd[0].Key != "claude-haiku-4-5" || bd[0].Requests != 2 {
		t.Fatalf("breakdown-by-model effective grouping wrong: %+v", bd)
	}
}

func f64(v float64) *float64 { return &v }

func TestPricingBaseUpsertNeverDeletes(t *testing.T) {
	s := openTest(t)
	if n, _ := s.CountPricingBase(); n != 0 {
		t.Fatalf("fresh base count = %d", n)
	}
	if err := s.UpsertPricingBase([]PricingBaseEntry{
		{Model: "claude-haiku-4-5", InputCost: f64(1e-6), OutputCost: f64(5e-6),
			CacheReadCost: f64(1e-7), LitellmProvider: "anthropic", Mode: "chat", Source: "snapshot"},
		{Model: "gpt-5.1-codex", InputCost: f64(1.25e-6), OutputCost: f64(1e-5),
			LitellmProvider: "openai", Mode: "responses", Source: "snapshot"},
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountPricingBase(); n != 2 {
		t.Fatalf("after import = %d, want 2", n)
	}

	// Second sync updates the price of one and adds none — never deletes the
	// other even though it is absent from this batch.
	if err := s.UpsertPricingBase([]PricingBaseEntry{
		{Model: "claude-haiku-4-5", InputCost: f64(2e-6), OutputCost: f64(6e-6),
			LitellmProvider: "anthropic", Mode: "chat", Source: "litellm"},
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountPricingBase(); n != 2 {
		t.Fatalf("upsert deleted rows: count = %d, want 2", n)
	}
	got, _ := s.GetPricingBase()
	prices := map[string]float64{}
	for _, e := range got {
		if e.InputCost != nil {
			prices[e.Model] = *e.InputCost
		}
	}
	if prices["claude-haiku-4-5"] != 2e-6 {
		t.Fatalf("price not updated: %v", prices["claude-haiku-4-5"])
	}
	// The OpenAI entry keeps its NULL cache-write price.
	for _, e := range got {
		if e.Model == "gpt-5.1-codex" && e.CacheWriteCost != nil {
			t.Fatal("openai cache_write should be NULL")
		}
	}
}

func TestPricingOverrides(t *testing.T) {
	s := openTest(t)
	if err := s.CreateProvider(Provider{ID: "p1", Name: "p1", Protocol: "anthropic",
		BaseURL: "https://x", APIKeyEnc: []byte{1}, ModelRedirects: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO pricing_overrides
		(provider_id, model, input_cost, output_cost) VALUES (?,?,?,?)`,
		"p1", "claude-haiku-4-5", 9e-6, 9e-5); err != nil {
		t.Fatal(err)
	}
	ovr, err := s.GetPricingOverrides()
	if err != nil || len(ovr) != 1 {
		t.Fatalf("overrides: %v len=%d", err, len(ovr))
	}
	if ovr[0].ProviderID != "p1" || ovr[0].InputCost == nil || *ovr[0].InputCost != 9e-6 {
		t.Fatalf("override wrong: %+v", ovr[0])
	}
	if ovr[0].CacheWriteCost != nil {
		t.Fatal("unset override column should be NULL")
	}
}
