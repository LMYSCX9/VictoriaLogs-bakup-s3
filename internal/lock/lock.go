// Package lock 提供基于本地锁文件的 cluster/node/date 互斥（设计文档 §8.2）。
// 锁采用 OS 级内核锁：Unix 用 Flock，Windows 用 LockFileEx，
// 进程死亡自动释放，无需 PID 存活检测，也不存在锁僵死问题。
package lock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// errLocked 是锁被其他进程持有时的底层原因，保证错误链完整。
var errLocked = errors.New("os-level file lock busy")

// Manager 管理一个节点（cluster/node 固定）对逐日期分区的作业锁，
// 锁文件位于 <dataPath>/.vlogs-backup/locks/<partition>-<cluster>-<node>.lock。
// 同一 Manager 内对同一分区的重复 Acquire 是幂等的。
type Manager struct {
	dir     string
	cluster string
	node    string

	mu   sync.Mutex
	held map[string]*handle // partition → 已持有的锁
}

type handle struct {
	f *os.File
}

// New 创建锁管理器并确保锁目录存在；cluster/node 参与锁文件命名，
// 保证同一数据目录上不同节点互不阻塞、同节点同日期严格互斥。
func New(dataPath, cluster, node string) (*Manager, error) {
	dir := filepath.Join(dataPath, ".vlogs-backup", "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.Untyped(err, "create lock dir %s", dir)
	}
	return &Manager{dir: dir, cluster: cluster, node: node, held: make(map[string]*handle)}, nil
}

// Dir 返回锁目录（供排障与测试）。
func (m *Manager) Dir() string { return m.dir }

// Acquire 获取 partition 的互斥锁；已被其他进程持有时返回前置条件错误（退出码 6）。
func (m *Manager) Acquire(partition string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.held[partition]; ok {
		return nil // 本进程已持有，幂等
	}
	path := m.lockPath(partition)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return errs.Untyped(err, "open lock file %s", path)
	}
	busy, lockErr := tryLock(f)
	if lockErr != nil {
		f.Close()
		return errs.Untyped(lockErr, "lock file %s", path)
	}
	if busy {
		f.Close()
		return errs.Precondition(errLocked, "another job holds lock for partition %s (lock file %s)", partition, path)
	}
	m.held[partition] = &handle{f: f}
	return nil
}

// Release 释放 partition 的锁；未持有时是 no-op。
// 锁文件保留在磁盘上（内容为空）：删除锁文件会在「释放-重建」窗口产生竞态，故不删。
func (m *Manager) Release(partition string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	h, ok := m.held[partition]
	if !ok {
		return nil
	}
	delete(m.held, partition)
	if err := unlock(h.f); err != nil {
		h.f.Close()
		return errs.Untyped(err, "unlock partition %s", partition)
	}
	if err := h.f.Close(); err != nil {
		return errs.Untyped(err, "close lock file for partition %s", partition)
	}
	return nil
}

// Held 报告本进程当前是否持有 partition 的锁。
// 孤立快照清理（设计文档 §7.6）据此判定快照是否属于仍存活的作业。
func (m *Manager) Held(partition string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.held[partition]
	return ok
}

// lockPath 生成锁文件路径：首段是 YYYYMMDD 分区（不含连字符），便于人工排查。
func (m *Manager) lockPath(partition string) string {
	name := sanitize(partition) + "-" + sanitize(m.cluster) + "-" + sanitize(m.node) + ".lock"
	return filepath.Join(m.dir, name)
}

// sanitize 把文件名中不安全字符替换为下划线。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "empty"
	}
	return out
}
