package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/backupmeta"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/lock"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
)

// ---------------------------------------------------------------------------
// 测试替身：fakeVL 实现 VLClient，fakeStore 实现 s3store.ObjectStore（§5.4 禁外网）。
// ---------------------------------------------------------------------------

var _ VLClient = (*fakeVL)(nil)
var _ s3store.ObjectStore = (*fakeStore)(nil)

type fakeVL struct {
	mu          sync.Mutex
	allSnaps    []string            // 全局快照视图（SnapshotCreate 注册、SnapshotDelete 摘除）
	created     map[string][]string // partition → SnapshotCreate 返回的路径
	createCalls int
	deleteErrs  map[string]error // path → 注入错误
	deleted     []string
	flushCalls  int
	probeErr    error
	version     string
}

func (f *fakeVL) Probe(ctx context.Context) error { return f.probeErr }

func (f *fakeVL) ForceFlush(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushCalls++
	return nil
}

func (f *fakeVL) SnapshotCreate(ctx context.Context, partition string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	paths := f.created[partition]
	f.allSnaps = append(f.allSnaps, paths...)
	out := make([]string, len(paths))
	copy(out, paths)
	return out, nil
}

func (f *fakeVL) SnapshotDelete(ctx context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.deleteErrs[path]; ok {
		return err
	}
	for i, p := range f.allSnaps {
		if p == path {
			f.allSnaps = append(f.allSnaps[:i], f.allSnaps[i+1:]...)
			break
		}
	}
	f.deleted = append(f.deleted, path)
	return nil
}

func (f *fakeVL) SnapshotList(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.allSnaps))
	copy(out, f.allSnaps)
	return out, nil
}

func (f *fakeVL) AppVersion(ctx context.Context) string { return f.version }

func (f *fakeVL) containsDeleted(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.deleted {
		if p == path {
			return true
		}
	}
	return false
}

type fakeStore struct {
	mu      sync.Mutex
	objs    map[string][]byte
	putErrs map[string]error
	listErr error
	puts    []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objs: map[string][]byte{}, putErrs: map[string]error{}}
}

func (f *fakeStore) Head(ctx context.Context, key string) (s3store.ObjectMeta, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objs[key]
	return s3store.ObjectMeta{Key: key, Size: int64(len(b)), LastModified: time.Now().UTC()}, ok, nil
}

func (f *fakeStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.putErrs[key]; ok {
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return errs.Untyped(err, "read upload body for %s", key)
	}
	f.objs[key] = b
	f.puts = append(f.puts, key)
	return nil
}

func (f *fakeStore) Get(ctx context.Context, key string, w io.WriterAt) error {
	return errors.New("not implemented in backup tests")
}

func (f *fakeStore) List(ctx context.Context, prefix string) ([]s3store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []s3store.ObjectMeta
	for k, b := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, s3store.ObjectMeta{Key: k, Size: int64(len(b))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeStore) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objs, key)
	return nil
}

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeSnapshotDir 构造真实 VL 快照目录（设计文档 §9）：
// <root>/partitions/<partition>/snapshots/<snapshotName>/，快照内容镜像分区结构
// （indexdb/…、datadb/…）；snapshotName 为 VL 生成的快照名，不是日期形态。
// files 的键为快照目录内相对路径（`/` 分隔）。返回注册进 fakeVL 的绝对路径。
func makeSnapshotDir(t *testing.T, root, partition, snapshotName string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, "partitions", partition, "snapshots", snapshotName)
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newTestJob(t *testing.T, fvl *fakeVL, store *fakeStore, locks *lock.Manager, mutate func(*Options)) (*Job, error) {
	t.Helper()
	opts := Options{
		Cluster:     "c1",
		Node:        "n1",
		ToolVersion: "test-1.0.0",
		Logger:      testLogger(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	return NewJob(fvl, store, locks, opts)
}

func newLocks(t *testing.T) *lock.Manager {
	t.Helper()
	return newLocksAt(t, t.TempDir())
}

func newLocksAt(t *testing.T, dataPath string) *lock.Manager {
	t.Helper()
	m, err := lock.New(dataPath, "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func testKeys(t *testing.T) *s3store.KeyBuilder {
	t.Helper()
	kb, err := s3store.NewKeyBuilder("", "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	return kb
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

func TestRunHappyPath(t *testing.T) {
	part := "20261001"
	s1 := makeSnapshotDir(t, t.TempDir(), part, "aa", map[string]string{"indexdb/part.0": "hello"})
	s2 := makeSnapshotDir(t, t.TempDir(), part, "bb", map[string]string{"datadb/sub/part.1": "world!!"})
	fvl := &fakeVL{created: map[string][]string{part: {s1, s2}}, version: "v1.99.0"}
	fs := newFakeStore()
	locks := newLocks(t)
	j, err := newTestJob(t, fvl, fs, locks, nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := j.Run(context.Background(), part)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Files != 2 || res.Bytes != int64(len("hello")+len("world!!")) {
		t.Fatalf("result files/bytes = %d/%d, want 2/%d", res.Files, res.Bytes, len("hello")+len("world!!"))
	}
	if !res.SnapshotCleaned {
		t.Fatal("snapshot must be cleaned (C6)")
	}
	if res.Today {
		t.Fatal("past partition must not force flush")
	}
	if locks.Held(part) {
		t.Fatal("lock must be released after run")
	}
	if len(fvl.deleted) != 2 {
		t.Fatalf("both snapshots must be deleted, got %v", fvl.deleted)
	}

	// manifest 内容校验（C3：唯一提交点）。
	kb := testKeys(t)
	mkey, err := kb.ManifestKey(part)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := fs.objs[mkey]
	if !ok {
		t.Fatalf("manifest %s must exist after successful run", mkey)
	}
	var got backupmeta.Manifest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("manifest is not valid json: %v", err)
	}
	if got.Partition != part || got.Cluster != "c1" || got.Node != "n1" {
		t.Fatalf("manifest identity mismatch: %+v", got)
	}
	if got.VLVersion != "v1.99.0" || got.ToolVersion != "test-1.0.0" {
		t.Fatalf("manifest version mismatch: %+v", got)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("manifest entries = %d, want 2", len(got.Entries))
	}
	wantPaths := map[string]bool{
		"indexdb/part.0":    false,
		"datadb/sub/part.1": false,
	}
	for _, e := range got.Entries {
		if _, ok := wantPaths[e.Path]; !ok {
			t.Fatalf("unexpected entry path %q", e.Path)
		}
		wantPaths[e.Path] = true
		if e.Size <= 0 || len(e.SHA256) != 64 {
			t.Fatalf("entry %q has bad size/sha256: %+v", e.Path, e)
		}
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Fatalf("entry %q missing from manifest", p)
		}
	}
}

func TestRunIdempotentResume(t *testing.T) {
	part := "20261001"
	s1 := makeSnapshotDir(t, t.TempDir(), part, "aa", map[string]string{"indexdb/part.0": "hello"})
	s2 := makeSnapshotDir(t, t.TempDir(), part, "bb", map[string]string{"datadb/part.0": "world"})
	fvl := &fakeVL{created: map[string][]string{part: {s1, s2}}, version: "v1.99.0"}
	fs := newFakeStore()
	locks := newLocks(t)

	// 预置已上传对象（大小一致，模拟上次中断的续传，R-BK-05）。
	kb := testKeys(t)
	pre1, err := kb.FileKey(part, "indexdb/part.0")
	if err != nil {
		t.Fatal(err)
	}
	pre2, err := kb.FileKey(part, "datadb/part.0")
	if err != nil {
		t.Fatal(err)
	}
	fs.objs[pre1] = []byte("hello")
	fs.objs[pre2] = []byte("world")

	j, err := newTestJob(t, fvl, fs, locks, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := j.Run(context.Background(), part)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", res.Skipped)
	}
	// 已存在对象不重传：只有 manifest 被新写入。
	for _, k := range fs.puts {
		if k == pre1 || k == pre2 {
			t.Fatalf("existing object %s must not be re-uploaded", k)
		}
	}
	mkey, err := kb.ManifestKey(part)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.objs[mkey]; !ok {
		t.Fatal("manifest must be committed")
	}
}

func TestRunSkipExisting(t *testing.T) {
	part := "20261001"
	kb := testKeys(t)
	mkey, err := kb.ManifestKey(part)
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeStore()
	fs.objs[mkey] = []byte(`{"format_version":1}`)
	fvl := &fakeVL{created: map[string][]string{}, version: "v1.99.0"}
	locks := newLocks(t)
	j, err := newTestJob(t, fvl, fs, locks, nil) // 默认 if-exists=skip
	if err != nil {
		t.Fatal(err)
	}
	res, err := j.Run(context.Background(), part)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.SkippedExisting {
		t.Fatal("result must mark skipped-existing")
	}
	if !res.SnapshotCleaned {
		t.Fatal("no snapshot created, cleaned flag must be true")
	}
	if fvl.createCalls != 0 {
		t.Fatalf("skip must not create snapshot, got %d create calls", fvl.createCalls)
	}
	if len(fs.puts) != 0 {
		t.Fatalf("skip must not upload anything, got %v", fs.puts)
	}
}

func TestRunReplace(t *testing.T) {
	part := "20261001"
	s1 := makeSnapshotDir(t, t.TempDir(), part, "aa", map[string]string{"datadb/part.0": "newdata"})
	fvl := &fakeVL{created: map[string][]string{part: {s1}}, version: "v1.99.0"}
	fs := newFakeStore()
	locks := newLocks(t)

	kb := testKeys(t)
	mkey, err := kb.ManifestKey(part)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := kb.FileKey(part, "20260930_zz/old.bin")
	if err != nil {
		t.Fatal(err)
	}
	fs.objs[mkey] = []byte(`{"format_version":1}`) // 旧 manifest（内容不求解析）
	fs.objs[stale] = []byte("stale-object")

	j, err := newTestJob(t, fvl, fs, locks, func(o *Options) { o.IfExists = IfExistsReplace })
	if err != nil {
		t.Fatal(err)
	}
	res, err := j.Run(context.Background(), part)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Replaced {
		t.Fatal("result must mark replaced")
	}
	if _, ok := fs.objs[stale]; ok {
		t.Fatal("stale object must be removed after replace")
	}
	raw, ok := fs.objs[mkey]
	if !ok {
		t.Fatal("new manifest must be committed")
	}
	var got backupmeta.Manifest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("new manifest invalid: %v", err)
	}
	if got.Partition != part {
		t.Fatalf("new manifest partition = %q", got.Partition)
	}
}

func TestRunUploadFailureKeepsNoManifest(t *testing.T) {
	part := "20261001"
	s1 := makeSnapshotDir(t, t.TempDir(), part, "aa", map[string]string{"indexdb/part.0": "hello"})
	fvl := &fakeVL{created: map[string][]string{part: {s1}}, version: "v1.99.0"}
	fs := newFakeStore()
	kb := testKeys(t)
	badKey, err := kb.FileKey(part, "indexdb/part.0")
	if err != nil {
		t.Fatal(err)
	}
	fs.putErrs[badKey] = errs.Network(errors.New("injected put failure"), "put %s", badKey)
	locks := newLocks(t)
	j, err := newTestJob(t, fvl, fs, locks, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := j.Run(context.Background(), part)
	if err == nil {
		t.Fatal("upload failure must fail the run")
	}
	if code := errs.ExitCode(err); code != 3 {
		t.Fatalf("exit code = %d, want 3 (network)", code)
	}
	mkey, err := kb.ManifestKey(part)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.objs[mkey]; ok {
		t.Fatal("manifest must not exist after upload failure (C3)")
	}
	// C6：失败分支同样必须清理快照。
	if !res.SnapshotCleaned {
		t.Fatal("snapshot must be cleaned even on failure (C6)")
	}
	if !fvl.containsDeleted(s1) {
		t.Fatalf("snapshot %s must be deleted on failure", s1)
	}
}

func TestRunTodayFlush(t *testing.T) {
	part := utcToday()
	s1 := makeSnapshotDir(t, t.TempDir(), part, "aa", map[string]string{"datadb/part.0": "hot"})
	fvl := &fakeVL{created: map[string][]string{part: {s1}}, version: "v1.99.0"}
	fs := newFakeStore()
	locks := newLocks(t)
	j, err := newTestJob(t, fvl, fs, locks, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := j.Run(context.Background(), part)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fvl.flushCalls != 1 {
		t.Fatalf("force flush calls = %d, want 1 (R-BK-04)", fvl.flushCalls)
	}
	if !res.Today {
		t.Fatal("today partition must be flagged")
	}
}

func TestRunSnapshotEmpty(t *testing.T) {
	part := "20261001"
	fvl := &fakeVL{created: map[string][]string{part: {}}, version: "v1.99.0"}
	fs := newFakeStore()
	locks := newLocks(t)
	j, err := newTestJob(t, fvl, fs, locks, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := j.Run(context.Background(), part)
	if err == nil {
		t.Fatal("empty snapshot must fail")
	}
	if code := errs.ExitCode(err); code != 4 {
		t.Fatalf("exit code = %d, want 4 (not found)", code)
	}
	if res.Files != 0 {
		t.Fatalf("files = %d, want 0", res.Files)
	}
	if len(fs.puts) != 0 {
		t.Fatalf("nothing must be uploaded, got %v", fs.puts)
	}
}

func TestRunLockBusy(t *testing.T) {
	part := "20261001"
	s1 := makeSnapshotDir(t, t.TempDir(), part, "aa", map[string]string{"indexdb/part.0": "x"})
	fvl := &fakeVL{created: map[string][]string{part: {s1}}, version: "v1.99.0"}
	fs := newFakeStore()
	dataPath := t.TempDir()
	locks := newLocksAt(t, dataPath)

	// 外部作业持有同一分区锁（独立 Manager + 同一锁目录）。
	ext, err := lock.New(dataPath, "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ext.Acquire(part); err != nil {
		t.Fatal(err)
	}
	defer ext.Release(part)

	j, err := newTestJob(t, fvl, fs, locks, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = j.Run(context.Background(), part)
	if err == nil {
		t.Fatal("busy lock must fail the run")
	}
	if code := errs.ExitCode(err); code != 6 {
		t.Fatalf("exit code = %d, want 6 (precondition)", code)
	}
	if len(fs.puts) != 0 {
		t.Fatalf("busy lock must not upload, got %v", fs.puts)
	}
}

func TestSweepOrphanSnapshots(t *testing.T) {
	// 三个候选快照分属三个分区：老（可清）、新（保守护栏）、被外部作业锁住（存活作业）。
	oldSnap := makeSnapshotDir(t, t.TempDir(), "20260901", "aa", map[string]string{"indexdb/part.0": "old"})
	youngSnap := makeSnapshotDir(t, t.TempDir(), "20260801", "bb", map[string]string{"indexdb/part.0": "young"})
	lockedSnap := makeSnapshotDir(t, t.TempDir(), "20260701", "cc", map[string]string{"indexdb/part.0": "locked"})
	lockedPart := "20260701"
	if err := os.Chtimes(oldSnap, time.Now().Add(-3*time.Hour), time.Now().Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}

	dataPath := t.TempDir()
	locks := newLocksAt(t, dataPath)
	// lockedSnap 所属分区被外部作业持有 → 不是孤立快照。
	ext, err := lock.New(dataPath, "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ext.Acquire(lockedPart); err != nil {
		t.Fatal(err)
	}
	defer ext.Release(lockedPart)

	fvl := &fakeVL{allSnaps: []string{oldSnap, youngSnap, lockedSnap}, version: "v1.99.0"}
	fs := newFakeStore()
	j, err := newTestJob(t, fvl, fs, locks, nil)
	if err != nil {
		t.Fatal(err)
	}
	j.sweepOrphanSnapshots(context.Background())

	if !fvl.containsDeleted(oldSnap) {
		t.Fatal("aged unlocked snapshot must be removed (R-BK-07)")
	}
	if fvl.containsDeleted(youngSnap) {
		t.Fatal("young snapshot must be kept (protection window)")
	}
	if fvl.containsDeleted(lockedSnap) {
		t.Fatal("snapshot of locked partition must be kept (running job)")
	}
}

func TestRunManyPartialFailure(t *testing.T) {
	ok1 := makeSnapshotDir(t, t.TempDir(), "20261001", "aa", map[string]string{"indexdb/part.0": "a"})
	ok2 := makeSnapshotDir(t, t.TempDir(), "20261002", "aa", map[string]string{"indexdb/part.0": "b"})
	fvl := &fakeVL{
		created: map[string][]string{
			"20261001": {ok1},
			"20261002": {ok2},
			// 20260930 无快照 → NotFound（退出码 4）
		},
		version: "v1.99.0",
	}
	fs := newFakeStore()
	locks := newLocks(t)
	j, err := newTestJob(t, fvl, fs, locks, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := j.RunMany(context.Background(), []string{"20260930", "20261001", "20261002"})
	if err == nil {
		t.Fatal("batch with a failed day must report worst error")
	}
	if code := errs.ExitCode(err); code != 4 {
		t.Fatalf("worst exit code = %d, want 4 (R-BK-02)", code)
	}
	if len(res) != 3 {
		t.Fatalf("partial results = %d, want 3", len(res))
	}
	mkey, err := testKeys(t).ManifestKey("20261001")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.objs[mkey]; !ok {
		t.Fatal("healthy days must still commit manifest")
	}
}

func TestExpandDates(t *testing.T) {
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		from    string
		to      string
		latest  int
		want    []string
		wantErr bool
	}{
		{name: "empty", want: nil},
		{name: "latest 3 days incl today", latest: 3, want: []string{"20261026", "20261027", "20261028"}},
		{name: "range", from: "20261001", to: "20261003", want: []string{"20261001", "20261002", "20261003"}},
		{name: "same day", from: "20261005", to: "20261005", want: []string{"20261005"}},
		{name: "latest with from", from: "20261001", latest: 2, wantErr: true},
		{name: "from after to", from: "20261003", to: "20261001", wantErr: true},
		{name: "from only", from: "20261001", wantErr: true},
		{name: "bad format", from: "2026-10-01", to: "20261003", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandDates(tc.from, tc.to, tc.latest, now)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				if code := errs.ExitCode(err); code != 2 {
					t.Fatalf("exit code = %d, want 2 (usage)", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
