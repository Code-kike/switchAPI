package usagebuf

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

func rec(id string) wire.UsageRecord {
	return wire.UsageRecord{RequestID: id, TS: time.Now().Unix(), App: "codex",
		InputTokens: 1, OutputTokens: 2, UsageSource: "wire", Status: 200}
}

// waitFor polls until cond holds or the deadline passes (the writer commits
// asynchronously).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func count(t *testing.T, q *Queue) int {
	t.Helper()
	n, err := q.PendingCount()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestEnqueuePersistAndReopenSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	q, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 5; i++ {
		q.Enqueue(rec(fmt.Sprintf("r-%d", i)))
	}
	waitFor(t, func() bool { return count(t, q) == 5 })
	// Duplicate request_id is ignored (idempotent).
	q.Enqueue(rec("r-0"))
	time.Sleep(50 * time.Millisecond)
	if n := count(t, q); n != 5 {
		t.Fatalf("after dup enqueue count = %d, want 5", n)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: the pending rows must still be there.
	q2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	if n := count(t, q2); n != 5 {
		t.Fatalf("after reopen count = %d, want 5 (queue did not survive restart)", n)
	}
}

func TestNextBatchAndAckDeletes(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i := 0; i < 3; i++ {
		q.Enqueue(rec(fmt.Sprintf("r-%d", i)))
	}
	waitFor(t, func() bool { return count(t, q) == 3 })

	batch, ok := q.NextBatch(100)
	if !ok || len(batch.Records) != 3 || batch.BatchID == "" {
		t.Fatalf("batch = %+v ok=%v", batch, ok)
	}
	// In-flight rows are excluded from the next batch.
	if _, ok := q.NextBatch(100); ok {
		t.Fatal("in-flight rows should not be handed out again")
	}
	q.Ack(batch.BatchID)
	if n := count(t, q); n != 0 {
		t.Fatalf("after ack count = %d, want 0", n)
	}
}

func TestResetInflightRequeues(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	q.Enqueue(rec("r-0"))
	q.Enqueue(rec("r-1"))
	waitFor(t, func() bool { return count(t, q) == 2 })

	batch, ok := q.NextBatch(100)
	if !ok || len(batch.Records) != 2 {
		t.Fatalf("batch = %+v ok=%v", batch, ok)
	}
	// Simulate a dropped connection before the ack.
	q.ResetInflight()
	batch2, ok := q.NextBatch(100)
	if !ok || len(batch2.Records) != 2 {
		t.Fatalf("after reset, records must be resendable: %+v ok=%v", batch2, ok)
	}
	if batch2.BatchID == batch.BatchID {
		t.Fatal("resend must use a fresh batch_id")
	}
	// Still zero deletions have happened.
	if n := count(t, q); n != 2 {
		t.Fatalf("reset must not delete rows, count = %d", n)
	}
}

func TestNextBatchRespectsMax(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i := 0; i < 10; i++ {
		q.Enqueue(rec(fmt.Sprintf("r-%d", i)))
	}
	waitFor(t, func() bool { return count(t, q) == 10 })

	batch, ok := q.NextBatch(4)
	if !ok || len(batch.Records) != 4 {
		t.Fatalf("batch size = %d, want 4", len(batch.Records))
	}
	// Records must be oldest-first.
	if batch.Records[0].RequestID != "r-0" || batch.Records[3].RequestID != "r-3" {
		t.Fatalf("ordering wrong: %s..%s", batch.Records[0].RequestID, batch.Records[3].RequestID)
	}
}

// Enqueue must never block, even under a flood far exceeding the buffer.
func TestEnqueueNeverBlocks(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			q.Enqueue(rec(fmt.Sprintf("flood-%d", i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked under flood — forwarding path would stall")
	}
}

// Concurrent producers must not lose records that fit in the buffer/db.
func TestConcurrentEnqueueNoLoss(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	const producers, each = 4, 20
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				q.Enqueue(rec(fmt.Sprintf("p%d-r%d", p, i)))
			}
		}(p)
	}
	wg.Wait()
	// Well under the 256 buffer → nothing dropped.
	waitFor(t, func() bool { return count(t, q) == producers*each })
}
