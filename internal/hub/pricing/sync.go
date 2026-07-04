package pricing

// sync.go — the daily LiteLLM refresh (研究#4 §e). A conditional GET with the
// stored ETag makes a no-change poll cost one empty 304; a 200 is filtered,
// upserted (never deleted), and the new ETag saved. The whole loop is a
// best-effort background task: any failure just retries next tick because the
// embedded snapshot guarantees a usable table at all times.

import (
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/store"
)

const (
	etagSettingKey    = "pricing_etag"
	enabledSettingKey = "pricing_sync_enabled"
	syncInterval      = 24 * time.Hour
)

// SyncDaily runs one sync immediately, then every 24h until ctx is cancelled.
// r is reloaded after any successful upsert so live queries see fresh prices.
func SyncDaily(ctx context.Context, st *store.Store, url string, r *Resolver) {
	if err := syncOnce(ctx, st, url, r); err != nil {
		log.Printf("pricing: initial sync: %v", err)
	}
	t := time.NewTicker(syncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := syncOnce(ctx, st, url, r); err != nil {
				log.Printf("pricing: sync: %v", err)
			}
		}
	}
}

// syncOnce performs a single conditional fetch + upsert. It returns nil (not an
// error) when disabled or unchanged — those are normal outcomes.
func syncOnce(ctx context.Context, st *store.Store, url string, r *Resolver) error {
	if !syncEnabled(st) {
		return nil
	}
	etag, _, err := st.GetSetting(etagSettingKey)
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil // unchanged since last sync
	}
	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	entries, err := parseTable(body, "litellm")
	if err != nil {
		return err
	}
	if err := st.UpsertPricingBase(entries); err != nil {
		return err
	}
	if newETag := resp.Header.Get("ETag"); newETag != "" {
		st.SetSetting(etagSettingKey, newETag)
	}
	if r != nil {
		if err := r.Reload(); err != nil {
			log.Printf("pricing: resolver reload after sync: %v", err)
		}
	}
	log.Printf("pricing: synced %d models from %s", len(entries), url)
	return nil
}

// syncEnabled reports the pricing_sync_enabled toggle; default enabled, "0"
// disables.
func syncEnabled(st *store.Store) bool {
	v, ok, err := st.GetSetting(enabledSettingKey)
	if err != nil || !ok {
		return true
	}
	return v != "0"
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string {
	return "pricing: unexpected status " + http.StatusText(e.code)
}
