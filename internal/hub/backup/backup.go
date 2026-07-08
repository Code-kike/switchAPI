// Package backup rotates database snapshots（父 design.md §7）：每日一次 +
// 结构性变更后防抖 5 分钟触发 `VACUUM INTO`，本地保留最近 10 份。
package backup

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/store"
)

const (
	keep       = 10
	debounce   = 5 * time.Minute
	dailyEvery = 24 * time.Hour
	filePrefix = "hub-"
	fileSuffix = ".db"
	// 毫秒精度：手动备份可能在同一秒内连续触发，秒级名会撞车（VACUUM INTO
	// 要求目标不存在）。
	timeLayoutFn = "20060102-150405.000"
)

// Manager owns the backups directory.
type Manager struct {
	st    *store.Store
	dir   string
	dirty chan struct{} // 容量 1：MarkDirty 合并触发
}

// New builds a manager storing snapshots under dir (created on demand).
func New(st *store.Store, dir string) *Manager {
	return &Manager{st: st, dir: dir, dirty: make(chan struct{}, 1)}
}

// MarkDirty schedules a debounced snapshot（API 在供应商/切换/序列/设备等
// 结构性写点调用；重复标脏在窗口内合并）.
func (m *Manager) MarkDirty() {
	select {
	case m.dirty <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is done: daily snapshots + debounced dirty snapshots.
func (m *Manager) Run(ctx context.Context) {
	daily := time.NewTicker(dailyEvery)
	defer daily.Stop()
	var pending <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-daily.C:
			m.snapshot("daily")
		case <-m.dirty:
			if pending == nil {
				pending = time.After(debounce)
			}
		case <-pending:
			pending = nil
			m.snapshot("change")
		}
	}
}

// Info describes one snapshot on disk.
type Info struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt int64  `json:"created_at"`
}

// RunNow takes a snapshot immediately (POST /backup/run) and rotates.
func (m *Manager) RunNow() (Info, error) {
	name, err := m.snapshot("manual")
	if err != nil {
		return Info{}, err
	}
	fi, err := os.Stat(filepath.Join(m.dir, name))
	if err != nil {
		return Info{}, err
	}
	return Info{Name: name, SizeBytes: fi.Size(), CreatedAt: fi.ModTime().Unix()}, nil
}

// List returns snapshots, newest first.
func (m *Manager) List() ([]Info, error) {
	entries, err := os.ReadDir(m.dir)
	if os.IsNotExist(err) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Info{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != fileSuffix {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{Name: e.Name(), SizeBytes: fi.Size(), CreatedAt: fi.ModTime().Unix()})
	}
	// 文件名内嵌零填充毫秒时间戳 → 字典序即时间序（mtime 只有秒精度，不可靠）。
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

func (m *Manager) snapshot(reason string) (string, error) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s%s-%s%s", filePrefix, time.Now().Format(timeLayoutFn), reason, fileSuffix)
	dest := filepath.Join(m.dir, name)
	if err := m.st.VacuumInto(dest); err != nil {
		log.Printf("backup: vacuum into %s: %v", dest, err)
		return "", err
	}
	m.rotate()
	log.Printf("backup: 快照完成 %s（%s）", name, reason)
	return name, nil
}

func (m *Manager) rotate() {
	list, err := m.List()
	if err != nil {
		return
	}
	for i := keep; i < len(list); i++ {
		os.Remove(filepath.Join(m.dir, list[i].Name))
	}
}
