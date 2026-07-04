// Package pricing is the Hub's cost engine (父 design.md §6, 研究#4). It seeds
// pricing_base from an embedded LiteLLM snapshot, keeps it fresh with a daily
// ETag-conditional sync (upsert-only — the upstream deletes retired models but
// our historical usage still needs their price), and resolves a per-request
// token quad to USD via the three-layer rule:
//
//	override[provider,model] ?? base[model] × coefficient(provider)
//
// The four token components settle independently; NULL price columns count 0
// (OpenAI has no cache-write price). An unmatched model costs 0 and is flagged
// unknown so the UI can render cost=null rather than a misleading 0.
package pricing

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/Code-kike/switchAPI/internal/hub/store"
)

// Prices holds the four per-token USD components.
type Prices struct {
	Input      float64
	Output     float64
	CacheWrite float64
	CacheRead  float64
}

// Usage is the rec-like token quad the engine prices.
type Usage struct {
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheWriteTokens int64
	CacheReadTokens  int64
}

// dateSuffix strips a trailing -YYYYMMDD snapshot date (研究#4 step 2). Dates
// are 20xx, so anchor on -20 to avoid clipping unrelated numeric suffixes.
var dateSuffix = regexp.MustCompile(`-20\d{6}$`)

// overrideKey packs (provider_id, model) into a map key.
func overrideKey(providerID, model string) string { return providerID + "\x00" + model }

// Resolver caches pricing_base + overrides in memory. Reload() rebuilds it
// after a sync; concurrent Resolve/Cost calls take the read lock.
type Resolver struct {
	st *store.Store

	mu        sync.RWMutex
	base      map[string]Prices // only models with ≥1 non-nil price
	overrides map[string]Prices // overrideKey → verbatim price
}

// NewResolver builds a Resolver and loads the current tables.
func NewResolver(st *store.Store) (*Resolver, error) {
	r := &Resolver{st: st}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload rebuilds the in-memory maps from the store (the post-sync hook).
func (r *Resolver) Reload() error {
	baseRows, err := r.st.GetPricingBase()
	if err != nil {
		return err
	}
	base := make(map[string]Prices, len(baseRows))
	for _, e := range baseRows {
		p, any := pricesFrom(e.InputCost, e.OutputCost, e.CacheWriteCost, e.CacheReadCost)
		if !any {
			continue // all-NULL row → treat model as unknown (design §3)
		}
		base[e.Model] = p
	}
	ovrRows, err := r.st.GetPricingOverrides()
	if err != nil {
		return err
	}
	overrides := make(map[string]Prices, len(ovrRows))
	for _, o := range ovrRows {
		p, _ := pricesFrom(o.InputCost, o.OutputCost, o.CacheWriteCost, o.CacheReadCost)
		overrides[overrideKey(o.ProviderID, o.Model)] = p
	}

	r.mu.Lock()
	r.base, r.overrides = base, overrides
	r.mu.Unlock()
	return nil
}

// pricesFrom converts nullable columns to Prices; any reports whether at least
// one component was non-nil.
func pricesFrom(in, out, cw, cr *float64) (Prices, bool) {
	var p Prices
	any := false
	if in != nil {
		p.Input = *in
		any = true
	}
	if out != nil {
		p.Output = *out
		any = true
	}
	if cw != nil {
		p.CacheWrite = *cw
		any = true
	}
	if cr != nil {
		p.CacheRead = *cr
		any = true
	}
	return p, any
}

// Resolve maps a wire model name to base prices via the four-step match:
// exact → strip -YYYYMMDD → lowercase last path segment (then exact/strip on
// it) → unknown.
func (r *Resolver) Resolve(model string) (Prices, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveLocked(model)
}

func (r *Resolver) resolveLocked(model string) (Prices, bool) {
	if p, ok := r.base[model]; ok { // step 1: exact
		return p, true
	}
	if stripped := dateSuffix.ReplaceAllString(model, ""); stripped != model { // step 2
		if p, ok := r.base[stripped]; ok {
			return p, true
		}
	}
	if i := strings.LastIndex(model, "/"); i >= 0 { // step 3: last path segment, lowercased
		tail := strings.ToLower(model[i+1:])
		if p, ok := r.base[tail]; ok {
			return p, true
		}
		if stripped := dateSuffix.ReplaceAllString(tail, ""); stripped != tail {
			if p, ok := r.base[stripped]; ok {
				return p, true
			}
		}
	}
	return Prices{}, false // step 4: unknown
}

// Override returns a verbatim per-provider price when one exists.
func (r *Resolver) Override(providerID, model string) (Prices, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.overrides[overrideKey(providerID, model)]
	return p, ok
}

// Cost settles one token quad. An override is used verbatim (it bypasses the
// coefficient); otherwise base × coeff per component. Returns known=false only
// when there is neither an override nor a base match — the caller then renders
// cost as null.
func (r *Resolver) Cost(u Usage, coeff float64, override *Prices) (float64, bool) {
	var p Prices
	if override != nil {
		p = *override
	} else {
		base, ok := r.Resolve(u.Model)
		if !ok {
			return 0, false
		}
		p = Prices{
			Input:      base.Input * coeff,
			Output:     base.Output * coeff,
			CacheWrite: base.CacheWrite * coeff,
			CacheRead:  base.CacheRead * coeff,
		}
	}
	usd := float64(u.InputTokens)*p.Input +
		float64(u.OutputTokens)*p.Output +
		float64(u.CacheWriteTokens)*p.CacheWrite +
		float64(u.CacheReadTokens)*p.CacheRead
	return usd, true
}

// ---- snapshot embed + load ----

//go:embed snapshot.json
var snapshotJSON []byte

// snapEntry is the per-model shape shared by the embedded snapshot and the raw
// upstream table; unread upstream fields are ignored.
type snapEntry struct {
	InputCost       *float64 `json:"input_cost_per_token"`
	OutputCost      *float64 `json:"output_cost_per_token"`
	CacheWriteCost  *float64 `json:"cache_creation_input_token_cost"`
	CacheReadCost   *float64 `json:"cache_read_input_token_cost"`
	LitellmProvider string   `json:"litellm_provider"`
	Mode            string   `json:"mode"`
}

// parseTable decodes a LiteLLM-shaped JSON object into store entries, keeping
// only mode∈{chat,responses} and skipping the _meta/sample_spec doc keys.
func parseTable(data []byte, source string) ([]store.PricingBaseEntry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]store.PricingBaseEntry, 0, len(raw))
	for model, rawSpec := range raw {
		if model == "sample_spec" || strings.HasPrefix(model, "_") {
			continue
		}
		var e snapEntry
		if err := json.Unmarshal(rawSpec, &e); err != nil {
			continue // non-object / unexpected shape → skip defensively
		}
		if e.Mode != "chat" && e.Mode != "responses" {
			continue
		}
		out = append(out, store.PricingBaseEntry{
			Model: model, InputCost: e.InputCost, OutputCost: e.OutputCost,
			CacheWriteCost: e.CacheWriteCost, CacheReadCost: e.CacheReadCost,
			LitellmProvider: e.LitellmProvider, Mode: e.Mode, Source: source,
		})
	}
	return out, nil
}

// EnsureLoaded seeds pricing_base from the embedded snapshot when it is empty
// (first boot). It is a no-op once rows exist.
func EnsureLoaded(st *store.Store) error {
	n, err := st.CountPricingBase()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	entries, err := parseTable(snapshotJSON, "snapshot")
	if err != nil {
		return err
	}
	return st.UpsertPricingBase(entries)
}
