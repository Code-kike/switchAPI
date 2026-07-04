// Package usagebuf is the Agent's durable, at-least-once usage queue. Parsed
// usage records land here (never blocking the forwarding path), get batched to
// the Hub over the WS channel, and are deleted only once the Hub acknowledges
// their batch. The queue is a single SQLite table so pending records survive
// process restarts and Hub outages (design.md §2; prd requirement 2/3).
package usagebuf

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/Code-kike/switchAPI/internal/shared/wire"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// enqueueBuffer bounds the in-memory hand-off before the single writer commits
// to SQLite; on overflow the newest record is dropped (with a log line) so the
// forwarding goroutine is never blocked.
const enqueueBuffer = 256

// Queue is the SQLite-backed pending-usage store plus its non-blocking writer.
type Queue struct {
	db   *sql.DB
	ch   chan wire.UsageRecord
	done chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	inflight map[string][]string // batch_id → request_ids handed to an unacked batch
}

// DefaultDBPath is ~/.switchapi/agent.db (alongside agent-state.json).
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".switchapi", "agent.db"), nil
}

// Open opens (creating if needed) the queue database at path. The directory is
// created 0700 and the file tightened to 0600 (it holds no secrets, but stays
// consistent with the Agent's other state — ADR-0005).
func Open(path string) (*Queue, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single-writer SQLite; keep the pool deterministic
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS pending(
		request_id TEXT PRIMARY KEY,
		payload    TEXT NOT NULL,
		created_at INTEGER NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)

	q := &Queue{
		db:       db,
		ch:       make(chan wire.UsageRecord, enqueueBuffer),
		done:     make(chan struct{}),
		inflight: make(map[string][]string),
	}
	q.wg.Add(1)
	go q.writer()
	return q, nil
}

// Close stops the writer (draining what it can) and closes the database.
func (q *Queue) Close() error {
	close(q.done)
	q.wg.Wait()
	return q.db.Close()
}

// writer is the single goroutine that persists records — INSERT OR IGNORE so a
// re-enqueued request_id (retry) is idempotent.
func (q *Queue) writer() {
	defer q.wg.Done()
	for {
		select {
		case <-q.done:
			q.drain()
			return
		case rec := <-q.ch:
			q.persist(rec)
		}
	}
}

func (q *Queue) drain() {
	for {
		select {
		case rec := <-q.ch:
			q.persist(rec)
		default:
			return
		}
	}
}

func (q *Queue) persist(rec wire.UsageRecord) {
	payload, err := json.Marshal(rec)
	if err != nil {
		log.Printf("usagebuf: 序列化用量失败 request_id=%s: %v", rec.RequestID, err)
		return
	}
	if _, err := q.db.Exec(
		`INSERT OR IGNORE INTO pending(request_id, payload, created_at) VALUES(?, ?, ?)`,
		rec.RequestID, string(payload), rec.TS); err != nil {
		log.Printf("usagebuf: 入队失败 request_id=%s: %v", rec.RequestID, err)
	}
}

// Enqueue hands a record to the writer without ever blocking the caller (the
// forwarding path). A full buffer drops the newest record with a log line —
// usage accounting is best-effort relative to never stalling a proxied request.
func (q *Queue) Enqueue(rec wire.UsageRecord) {
	if rec.RequestID == "" {
		return
	}
	select {
	case <-q.done:
		return // closing
	case q.ch <- rec:
	default:
		log.Printf("usagebuf: 队列缓冲已满，丢弃用量 request_id=%s（转发不受影响）", rec.RequestID)
	}
}

// PendingCount returns the number of rows still in the queue (including any
// currently handed to an unacked batch). Used by the batch pump's threshold.
func (q *Queue) PendingCount() (int, error) {
	var n int
	err := q.db.QueryRow(`SELECT COUNT(*) FROM pending`).Scan(&n)
	return n, err
}

// NextBatch assembles up to max pending records that are not already part of an
// in-flight batch, tags them with a fresh batch_id, and remembers the mapping
// so Ack can delete exactly those rows. Returns ok=false when nothing is
// available. Records are ordered oldest-first (created_at).
func (q *Queue) NextBatch(max int) (wire.UsageBatch, bool) {
	if max <= 0 {
		max = 100
	}
	q.mu.Lock()
	inflightSet := make(map[string]bool)
	for _, ids := range q.inflight {
		for _, id := range ids {
			inflightSet[id] = true
		}
	}
	q.mu.Unlock()

	rows, err := q.db.Query(`SELECT request_id, payload FROM pending ORDER BY created_at`)
	if err != nil {
		log.Printf("usagebuf: 读取队列失败: %v", err)
		return wire.UsageBatch{}, false
	}
	var recs []wire.UsageRecord
	var ids []string
	for rows.Next() {
		if len(recs) >= max {
			break
		}
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			continue
		}
		if inflightSet[id] {
			continue
		}
		var rec wire.UsageRecord
		if json.Unmarshal([]byte(payload), &rec) != nil {
			continue
		}
		recs = append(recs, rec)
		ids = append(ids, id)
	}
	rows.Close()

	if len(recs) == 0 {
		return wire.UsageBatch{}, false
	}
	batchID := uuid.NewString()
	q.mu.Lock()
	q.inflight[batchID] = ids
	q.mu.Unlock()
	return wire.UsageBatch{BatchID: batchID, Records: recs}, true
}

// Ack deletes the rows of an acknowledged batch and forgets its mapping.
// Unknown batch ids (already acked / reset) are ignored.
func (q *Queue) Ack(batchID string) {
	q.mu.Lock()
	ids := q.inflight[batchID]
	delete(q.inflight, batchID)
	q.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	tx, err := q.db.Begin()
	if err != nil {
		log.Printf("usagebuf: ack 事务失败 batch=%s: %v", batchID, err)
		return
	}
	stmt, err := tx.Prepare(`DELETE FROM pending WHERE request_id = ?`)
	if err != nil {
		tx.Rollback()
		log.Printf("usagebuf: ack 预编译失败: %v", err)
		return
	}
	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			log.Printf("usagebuf: ack 删除失败 request_id=%s: %v", id, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		log.Printf("usagebuf: ack 提交失败 batch=%s: %v", batchID, err)
	}
}

// ResetInflight returns every in-flight batch to the pending pool. Called on
// (re)connect so records handed to a batch that was never acked (connection
// died) become eligible for resend — the at-least-once guarantee.
func (q *Queue) ResetInflight() {
	q.mu.Lock()
	q.inflight = make(map[string][]string)
	q.mu.Unlock()
}
