package lock

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// mustNew 在 dir 下创建 Manager，并在测试结束时释放其全部锁（Windows 下
// TempDir 清理要求无未关闭句柄）。
func mustNew(t *testing.T, dir, cluster, node string) *Manager {
	t.Helper()
	m, err := New(dir, cluster, node)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Release("20261027") })
	return m
}

func TestAcquireExclusive(t *testing.T) {
	dir := t.TempDir()
	m1 := mustNew(t, dir, "prod", "node-0")
	m2 := mustNew(t, dir, "prod", "node-0")

	if err := m1.Acquire("20261027"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !m1.Held("20261027") {
		t.Fatal("m1 should hold partition")
	}

	// 并发第二个作业：必须拿到退出码 6（T2.4 验收）。
	err := m2.Acquire("20261027")
	if err == nil {
		t.Fatal("second acquire should fail while first holds the lock")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("error should be *errs.Error, got %T: %v", err, err)
	}
	if got := errs.ExitCode(err); got != errs.ExitPrecondition {
		t.Fatalf("exit code = %d, want %d (precondition)", got, errs.ExitPrecondition)
	}
	if m2.Held("20261027") {
		t.Fatal("m2 must not record the partition as held")
	}

	// 释放后可重入。
	if err := m1.Release("20261027"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if m1.Held("20261027") {
		t.Fatal("m1 should not hold partition after release")
	}
	if err := m2.Acquire("20261027"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := m2.Release("20261027"); err != nil {
		t.Fatalf("release 2: %v", err)
	}
	if m2.Held("20261027") {
		t.Fatal("m2 should not hold partition after release")
	}
}

func TestAcquireIdempotentWithinManager(t *testing.T) {
	m := mustNew(t, t.TempDir(), "prod", "node-0")
	if err := m.Acquire("20261027"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := m.Acquire("20261027"); err != nil {
		t.Fatalf("re-acquire should be idempotent: %v", err)
	}
	if err := m.Release("20261027"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := m.Release("20261027"); err != nil {
		t.Fatalf("release when not held should be no-op: %v", err)
	}
	if m.Held("20261027") {
		t.Fatal("held must be false after release")
	}
}

func TestDifferentIdentityIndependent(t *testing.T) {
	dir := t.TempDir()
	a := mustNew(t, dir, "prod", "node-0")
	b := mustNew(t, dir, "prod", "node-1")
	if err := a.Acquire("20261027"); err != nil {
		t.Fatalf("acquire node-0: %v", err)
	}
	if err := b.Acquire("20261027"); err != nil {
		t.Fatalf("node-1 should not be blocked by node-0 lock: %v", err)
	}
	if err := a.Release("20261027"); err != nil {
		t.Fatalf("release node-0: %v", err)
	}
	if err := b.Release("20261027"); err != nil {
		t.Fatalf("release node-1: %v", err)
	}
}

func TestLockFileLayout(t *testing.T) {
	m := mustNew(t, t.TempDir(), "prod", "vl-node.1")
	if err := m.Acquire("20261027"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// 锁文件名：<partition>-<cluster>-<node>.lock，不安全字符已消毒。
	want := filepath.Join(m.Dir(), "20261027-prod-vl-node.1.lock")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("lock file %s should exist: %v", want, err)
	}
}

func TestConcurrentManagersOnlyOneWins(t *testing.T) {
	// 8 个独立 Manager（模拟 8 个进程）争抢同一分区：恰好一个成功。
	dir := t.TempDir()
	const partition = "20261027"
	const workers = 8

	managers := make([]*Manager, workers)
	for i := range workers {
		managers[i] = mustNew(t, dir, "prod", "node-0")
	}

	var wg sync.WaitGroup
	wins := make(chan int, workers)
	for i, m := range managers {
		wg.Add(1)
		go func(i int, m *Manager) {
			defer wg.Done()
			if err := m.Acquire(partition); err == nil {
				wins <- i
			}
		}(i, m)
	}
	wg.Wait()
	close(wins)

	winners := make([]int, 0, workers)
	for i := range wins {
		winners = append(winners, i)
	}
	if len(winners) != 1 {
		t.Fatalf("exactly one concurrent acquirer should win, got %d (%v)", len(winners), winners)
	}
	if err := managers[winners[0]].Release(partition); err != nil {
		t.Fatalf("release winner: %v", err)
	}
	// 释放后其他 Manager 可以拿到。
	if err := managers[(winners[0]+1)%workers].Acquire(partition); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}
