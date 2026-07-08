package backup

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/store"
)

func TestSnapshotAndRotation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, filepath.Join(dir, "backups"))

	var last Info
	for i := 0; i < 12; i++ {
		info, err := m.RunNow()
		if err != nil {
			t.Fatal(err)
		}
		if info.SizeBytes == 0 {
			t.Fatal("empty snapshot")
		}
		last = info
		time.Sleep(2 * time.Millisecond) // 毫秒级文件名，间隔即可去重
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) > keep {
		t.Fatalf("rotation kept %d > %d", len(list), keep)
	}
	if list[0].Name != last.Name {
		t.Fatalf("newest first violated: %s vs %s", list[0].Name, last.Name)
	}
	// 快照是合法 SQLite 库：能被打开且含 schema。
	snap, err := store.Open(filepath.Join(dir, "backups", last.Name))
	if err != nil {
		t.Fatalf("snapshot not a valid db: %v", err)
	}
	snap.Close()
}
