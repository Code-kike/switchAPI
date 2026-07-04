package api

import (
	"encoding/json"
	"testing"

	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

// seedPricing loads a couple of known base prices into the rig's store and
// reloads the resolver so /stats can settle cost.
func (r *testRig) seedPricing(t *testing.T) {
	t.Helper()
	i, o, cr := 1e-6, 5e-6, 1e-7
	if err := r.st.UpsertPricingBase([]store.PricingBaseEntry{
		{Model: "claude-haiku-4-5", InputCost: &i, OutputCost: &o, CacheReadCost: &cr,
			LitellmProvider: "anthropic", Mode: "chat", Source: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	// The rig's resolver was built at construction; force a reload. It is the
	// same instance wired into the server.
	r.reloadPricer(t)
}

func TestUsageDetailAndCost(t *testing.T) {
	r := newTestRig(t)
	pA := r.createProvider(t, "站点A", "anthropic", "sk-a-0001")
	r.seedPricing(t)

	// Seed usage directly: one priceable (known model), one unknown model.
	recs := []wire.UsageRecord{
		{RequestID: "r1", TS: 1000, App: "claude-code", ProviderID: pA,
			Model: "claude-haiku-4-5", InputTokens: 1000, OutputTokens: 500,
			CacheReadTokens: 200, Status: 200, UsageSource: "wire"},
		{RequestID: "r2", TS: 2000, App: "claude-code", ProviderID: pA,
			Model: "ZhipuAI/GLM-5.2", InputTokens: 100, OutputTokens: 50,
			Status: 200, UsageSource: "wire"},
	}
	if _, err := r.st.InsertUsageRecords("d1", recs); err != nil {
		t.Fatal(err)
	}

	code, body := r.do(r.auth, "GET", "/api/v1/usage?from=1&to=9999", "")
	if code != 200 {
		t.Fatalf("usage = %d: %s", code, body)
	}
	var resp struct {
		Total int `json:"total"`
		Rows  []struct {
			Model string   `json:"model"`
			Cost  *float64 `json:"cost"`
		} `json:"rows"`
	}
	json.Unmarshal(body, &resp)
	if resp.Total != 2 || len(resp.Rows) != 2 {
		t.Fatalf("usage total=%d rows=%d", resp.Total, len(resp.Rows))
	}
	// Rows newest-first: r2 (unknown model) then r1 (known).
	if resp.Rows[0].Model != "ZhipuAI/GLM-5.2" || resp.Rows[0].Cost != nil {
		t.Fatalf("unknown model should have null cost: %+v", resp.Rows[0])
	}
	wantCost := 1000*1e-6 + 500*5e-6 + 200*1e-7
	if resp.Rows[1].Cost == nil || *resp.Rows[1].Cost != wantCost {
		t.Fatalf("known cost = %v, want %v", resp.Rows[1].Cost, wantCost)
	}
}

func TestUsageRedirectedModelPricesAgainstTarget(t *testing.T) {
	r := newTestRig(t)
	pA := r.createProvider(t, "站点A", "anthropic", "sk-a-0001")
	r.seedPricing(t) // seeds claude-haiku-4-5 (the redirect target)

	// Requested a retired model; redirected to the live target that has a price.
	rec := wire.UsageRecord{
		RequestID: "r1", TS: 1000, App: "claude-code", ProviderID: pA,
		Model: "claude-3-5-haiku-20241022", ModelRedirected: "claude-haiku-4-5",
		InputTokens: 1000, OutputTokens: 500, Status: 200, UsageSource: "wire",
	}
	if _, err := r.st.InsertUsageRecords("d1", []wire.UsageRecord{rec}); err != nil {
		t.Fatal(err)
	}
	_, body := r.do(r.auth, "GET", "/api/v1/usage?from=1&to=9999", "")
	var resp struct {
		Rows []struct {
			Cost *float64 `json:"cost"`
		} `json:"rows"`
	}
	json.Unmarshal(body, &resp)
	// Prices against the target, not the retired requested name (which is unknown).
	want := 1000*1e-6 + 500*5e-6
	if len(resp.Rows) != 1 || resp.Rows[0].Cost == nil || *resp.Rows[0].Cost != want {
		t.Fatalf("redirected row cost = %v, want %v", resp.Rows, want)
	}
}

func TestUsageFilters(t *testing.T) {
	r := newTestRig(t)
	pA := r.createProvider(t, "A", "anthropic", "sk-a-0001")
	pO := r.createProvider(t, "O", "openai", "sk-o-0001")
	recs := []wire.UsageRecord{
		{RequestID: "r1", TS: 1000, App: "claude-code", ProviderID: pA, Model: "m1", Status: 200, UsageSource: "wire"},
		{RequestID: "r2", TS: 2000, App: "codex", ProviderID: pO, Model: "m2", Status: 200, UsageSource: "wire"},
	}
	if _, err := r.st.InsertUsageRecords("d1", recs); err != nil {
		t.Fatal(err)
	}
	_, body := r.do(r.auth, "GET", "/api/v1/usage?from=1&to=9999&app=codex", "")
	var resp struct {
		Total int `json:"total"`
	}
	json.Unmarshal(body, &resp)
	if resp.Total != 1 {
		t.Fatalf("app filter total = %d, want 1", resp.Total)
	}
	_, body = r.do(r.auth, "GET", "/api/v1/usage?from=1&to=9999&provider_id="+pA, "")
	json.Unmarshal(body, &resp)
	if resp.Total != 1 {
		t.Fatalf("provider filter total = %d, want 1", resp.Total)
	}
}

func TestStatsSummaryTrendBreakdown(t *testing.T) {
	r := newTestRig(t)
	pA := r.createProvider(t, "站点A", "anthropic", "sk-a-0001")
	r.seedPricing(t)
	recs := []wire.UsageRecord{
		{RequestID: "r1", TS: 3600, App: "claude-code", ProviderID: pA,
			Model: "claude-haiku-4-5", InputTokens: 1000, OutputTokens: 500, Status: 200, UsageSource: "wire"},
		{RequestID: "r2", TS: 3700, App: "claude-code", ProviderID: pA,
			Model: "claude-haiku-4-5", InputTokens: 1000, OutputTokens: 500, Status: 200, UsageSource: "wire"},
		{RequestID: "r3", TS: 7200, App: "codex", ProviderID: pA,
			Model: "ZhipuAI/GLM-5.2", InputTokens: 100, OutputTokens: 50, Status: 200, UsageSource: "wire"},
	}
	if _, err := r.st.InsertUsageRecords("d1", recs); err != nil {
		t.Fatal(err)
	}
	perReq := 1000*1e-6 + 500*5e-6

	// Summary: grand totals + per-app split; unknown model → cost_unknown_requests.
	code, body := r.do(r.auth, "GET", "/api/v1/stats/summary?from=1&to=99999", "")
	if code != 200 {
		t.Fatalf("summary = %d: %s", code, body)
	}
	var sum struct {
		Requests            int64    `json:"requests"`
		InputTokens         int64    `json:"input_tokens"`
		Cost                *float64 `json:"cost"`
		CostUnknownRequests int64    `json:"cost_unknown_requests"`
		ByApp               map[string]struct {
			Requests int64    `json:"requests"`
			Cost     *float64 `json:"cost"`
		} `json:"by_app"`
	}
	json.Unmarshal(body, &sum)
	if sum.Requests != 3 || sum.InputTokens != 2100 {
		t.Fatalf("summary totals: %+v", sum)
	}
	if sum.Cost == nil || *sum.Cost != 2*perReq {
		t.Fatalf("summary cost = %v, want %v", sum.Cost, 2*perReq)
	}
	if sum.CostUnknownRequests != 1 {
		t.Fatalf("cost_unknown_requests = %d, want 1", sum.CostUnknownRequests)
	}
	if sum.ByApp["claude-code"].Requests != 2 || sum.ByApp["codex"].Requests != 1 {
		t.Fatalf("by_app split wrong: %+v", sum.ByApp)
	}
	if sum.ByApp["codex"].Cost != nil {
		t.Fatal("codex app cost should be null (unknown model)")
	}

	// Trend by hour: two buckets.
	_, body = r.do(r.auth, "GET", "/api/v1/stats/trend?from=1&to=99999&bucket=hour", "")
	var trend []struct {
		BucketTS int64 `json:"bucket_ts"`
		Requests int64 `json:"requests"`
	}
	json.Unmarshal(body, &trend)
	if len(trend) != 2 || trend[0].BucketTS != 3600 || trend[0].Requests != 2 {
		t.Fatalf("trend = %+v", trend)
	}

	// Breakdown by provider: single provider, name joined.
	_, body = r.do(r.auth, "GET", "/api/v1/stats/breakdown?from=1&to=99999&by=provider", "")
	var bd []struct {
		Key      string `json:"key"`
		Name     string `json:"name"`
		Requests int64  `json:"requests"`
	}
	json.Unmarshal(body, &bd)
	if len(bd) != 1 || bd[0].Key != pA || bd[0].Name != "站点A" || bd[0].Requests != 3 {
		t.Fatalf("breakdown = %+v", bd)
	}

	// Bad breakdown dimension → 400.
	if code, _ := r.do(r.auth, "GET", "/api/v1/stats/breakdown?by=bogus", ""); code != 400 {
		t.Fatalf("bad breakdown dim = %d, want 400", code)
	}
}

func TestStatsOverrideBypassesCoefficient(t *testing.T) {
	r := newTestRig(t)
	// Provider with a 0.5 coefficient, plus a per-model override that must win.
	pA := r.createProvider(t, "站点A", "anthropic", "sk-a-0001")
	if code, _ := r.do(r.auth, "PUT", "/api/v1/providers/"+pA,
		`{"cost_coefficient":0.5}`); code != 200 {
		t.Fatalf("set coefficient failed")
	}
	r.seedPricing(t)
	if _, err := r.st.DB().Exec(`INSERT INTO pricing_overrides
		(provider_id, model, input_cost, output_cost) VALUES (?,?,?,?)`,
		pA, "claude-haiku-4-5", 1e-3, 2e-3); err != nil {
		t.Fatal(err)
	}
	r.reloadPricer(t)

	if _, err := r.st.InsertUsageRecords("d1", []wire.UsageRecord{
		{RequestID: "r1", TS: 1000, App: "claude-code", ProviderID: pA,
			Model: "claude-haiku-4-5", InputTokens: 100, OutputTokens: 50,
			Status: 200, UsageSource: "wire"},
	}); err != nil {
		t.Fatal(err)
	}
	_, body := r.do(r.auth, "GET", "/api/v1/stats/summary?from=1&to=9999", "")
	var sum struct {
		Cost *float64 `json:"cost"`
	}
	json.Unmarshal(body, &sum)
	// Override is verbatim: 100*1e-3 + 50*2e-3, coefficient 0.5 ignored.
	want := 100*1e-3 + 50*2e-3
	if sum.Cost == nil || *sum.Cost != want {
		t.Fatalf("override cost = %v, want %v (coeff must be bypassed)", sum.Cost, want)
	}
}
