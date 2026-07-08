package store

// pricing.go — DAOs for the LiteLLM price tables (研究#4). pricing_base is the
// global snapshot (upsert-only, never deleted — the upstream removes retired
// models but our historical usage still needs their price); pricing_overrides
// is the per-provider manual price. All four cost columns are nullable REAL
// (OpenAI has no cache-write price → NULL, counted as 0 by the engine).

import "time"

// PricingBaseEntry mirrors one pricing_base row. Nil cost pointers are stored
// as SQL NULL and treated as 0 at settlement time.
type PricingBaseEntry struct {
	Model           string
	InputCost       *float64
	OutputCost      *float64
	CacheWriteCost  *float64
	CacheReadCost   *float64
	LitellmProvider string
	Mode            string
	TieredPrices    string // JSON text, "" when absent
	Source          string // "snapshot" | "litellm"
}

// PricingOverride mirrors one pricing_overrides row.
type PricingOverride struct {
	ProviderID     string
	Model          string
	InputCost      *float64
	OutputCost     *float64
	CacheWriteCost *float64
	CacheReadCost  *float64
}

// CountPricingBase reports how many base rows exist (EnsureLoaded gate).
func (s *Store) CountPricingBase() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pricing_base`).Scan(&n)
	return n, err
}

// UpsertPricingBase inserts or updates each entry in a single transaction.
// It NEVER deletes — retired upstream models keep their price for historical
// records. synced_at is stamped now.
func (s *Store) UpsertPricingBase(entries []PricingBaseEntry) error {
	if len(entries) == 0 {
		return nil
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO pricing_base
		(model, input_cost, output_cost, cache_write_cost, cache_read_cost,
		 litellm_provider, mode, tiered_prices, source, synced_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(model) DO UPDATE SET
			input_cost=excluded.input_cost,
			output_cost=excluded.output_cost,
			cache_write_cost=excluded.cache_write_cost,
			cache_read_cost=excluded.cache_read_cost,
			litellm_provider=excluded.litellm_provider,
			mode=excluded.mode,
			tiered_prices=excluded.tiered_prices,
			source=excluded.source,
			synced_at=excluded.synced_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		var tiered any
		if e.TieredPrices != "" {
			tiered = e.TieredPrices
		}
		if _, err := stmt.Exec(e.Model, e.InputCost, e.OutputCost, e.CacheWriteCost,
			e.CacheReadCost, e.LitellmProvider, e.Mode, tiered, e.Source, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetPricingBase returns every base row (the Resolver's in-memory source).
func (s *Store) GetPricingBase() ([]PricingBaseEntry, error) {
	rows, err := s.db.Query(`SELECT model, input_cost, output_cost, cache_write_cost,
		cache_read_cost, litellm_provider, mode, COALESCE(tiered_prices, ''), source
		FROM pricing_base`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PricingBaseEntry
	for rows.Next() {
		var e PricingBaseEntry
		if err := rows.Scan(&e.Model, &e.InputCost, &e.OutputCost, &e.CacheWriteCost,
			&e.CacheReadCost, &e.LitellmProvider, &e.Mode, &e.TieredPrices, &e.Source); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetPricingOverrides returns every override row across all providers.
func (s *Store) GetPricingOverrides() ([]PricingOverride, error) {
	rows, err := s.db.Query(`SELECT provider_id, model, input_cost, output_cost,
		cache_write_cost, cache_read_cost FROM pricing_overrides`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PricingOverride
	for rows.Next() {
		var o PricingOverride
		if err := rows.Scan(&o.ProviderID, &o.Model, &o.InputCost, &o.OutputCost,
			&o.CacheWriteCost, &o.CacheReadCost); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UpsertPricingOverrides replaces/creates override rows（导入还原用）。
func (s *Store) UpsertPricingOverrides(rows []PricingOverride) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, o := range rows {
		if _, err := tx.Exec(`INSERT INTO pricing_overrides
			(provider_id, model, input_cost, output_cost, cache_write_cost, cache_read_cost)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT(provider_id, model) DO UPDATE SET input_cost=excluded.input_cost,
			output_cost=excluded.output_cost, cache_write_cost=excluded.cache_write_cost,
			cache_read_cost=excluded.cache_read_cost`,
			o.ProviderID, o.Model, o.InputCost, o.OutputCost, o.CacheWriteCost, o.CacheReadCost); err != nil {
			return err
		}
	}
	return tx.Commit()
}
