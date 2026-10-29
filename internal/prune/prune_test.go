package prune

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
)

// ---- 故障注入基建 ----

type fakeStore struct {
	objs        map[string]s3store.ObjectMeta
	listErr     error
	deleteErrs  map[string]error
	deleteCalls []string
}

func (f *fakeStore) List(_ context.Context, prefix string) ([]s3store.ObjectMeta, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []s3store.ObjectMeta
	for k, o := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeStore) Delete(_ context.Context, key string) error {
	f.deleteCalls = append(f.deleteCalls, key)
	if e, ok := f.deleteErrs[key]; ok {
		return e
	}
	delete(f.objs, key)
	return nil
}

type testEnv struct {
	t     *testing.T
	store *fakeStore
	keys  *s3store.KeyBuilder
	job   *Job
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	store := &fakeStore{objs: map[string]s3store.ObjectMeta{}, deleteErrs: map[string]error{}}
	keys, err := s3store.NewKeyBuilder("vlogs", "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := NewJob(store, keys, Options{
		StaleAfter: 24 * time.Hour,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, store: store, keys: keys, job: job}
}

func (e *testEnv) addObj(key string, size int64, age time.Duration) {
	e.t.Helper()
	e.store.objs[key] = s3store.ObjectMeta{
		Key: key, Size: size,
		LastModified: time.Now().Add(-age),
	}
}

// seed 构造三个单元：complete（有 manifest，旧）、stale（无 manifest，旧）、fresh（无 manifest，新）。
func (e *testEnv) seed() {
	const (
		old = 48 * time.Hour
		new = 1 * time.Hour
	)
	e.addObj("vlogs/v1/c1/n1/20260101/manifest.json", 500, old)
	e.addObj("vlogs/v1/c1/n1/20260101/files/part.0", 1000, old)
	e.addObj("vlogs/v1/c1/n1/20260101/files/part.1", 2000, old)

	e.addObj("vlogs/v1/c1/n1/20260102/files/part.0", 3000, old)
	e.addObj("vlogs/v1/c1/n1/20260102/files/part.1", 4000, old)

	e.addObj("vlogs/v1/c1/n1/20260103/files/part.0", 5000, new)
}

// ---- Scan（R-GC-03/04）----

func TestScanStaleUnits(t *testing.T) {
	env := newTestEnv(t)
	env.seed()

	plan, err := env.job.Scan(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Units) != 1 {
		t.Fatalf("want exactly 1 stale unit, got %+v", plan.Units)
	}
	u := plan.Units[0]
	if u.Partition != "20260102" {
		t.Fatalf("stale unit = %q, want 20260102 (complete unit and fresh prefix must be skipped)", u.Partition)
	}
	if u.Objects != 2 || u.Bytes != 7000 {
		t.Fatalf("unexpected scale: objects=%d bytes=%d", u.Objects, u.Bytes)
	}
	if plan.TotalObjects != 2 || plan.TotalBytes != 7000 {
		t.Fatalf("unexpected plan totals: %+v", plan)
	}
	if !u.LastModified.After(time.Now().Add(-49 * time.Hour)) {
		t.Fatalf("LastModified must reflect max object age: %v", u.LastModified)
	}
}

func TestScanEmpty(t *testing.T) {
	env := newTestEnv(t)
	plan, err := env.job.Scan(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Units) != 0 {
		t.Fatalf("want empty plan, got %+v", plan.Units)
	}
}

func TestScanJunkKeyIgnored(t *testing.T) {
	env := newTestEnv(t)
	env.addObj("vlogs/v1/c1/n1/not-a-date/stray", 10, 48*time.Hour)
	plan, err := env.job.Scan(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Units) != 0 {
		t.Fatalf("junk segment must be ignored, got %+v", plan.Units)
	}
}

func TestScanListFailure(t *testing.T) {
	env := newTestEnv(t)
	env.store.listErr = errors.New("network split")
	_, err := env.job.Scan(context.Background(), 24*time.Hour)
	if errs.ExitCode(err) != errs.ExitNetwork {
		t.Fatalf("want exit 3, got %v", err)
	}
}

// ---- Run：dry-run 默认 / yes 执行（R-GC-01）----

func TestRunDryRunDeletesNothing(t *testing.T) {
	env := newTestEnv(t)
	env.seed()

	res, err := env.job.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun {
		t.Fatal("yes=false must be dry-run")
	}
	if len(res.Plan.Units) != 1 {
		t.Fatalf("plan must list the stale unit, got %+v", res.Plan.Units)
	}
	if len(env.store.deleteCalls) != 0 {
		t.Fatalf("dry-run must not delete anything, got %v", env.store.deleteCalls)
	}
	if len(res.Deleted) != 0 || res.Objects != 0 {
		t.Fatalf("dry-run must not report deletions: %+v", res)
	}
}

func TestRunExecuteDeletesStaleOnly(t *testing.T) {
	env := newTestEnv(t)
	env.seed()

	res, err := env.job.Run(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.DryRun {
		t.Fatal("yes=true must not be dry-run")
	}
	if len(res.Deleted) != 1 || res.Deleted[0].Partition != "20260102" {
		t.Fatalf("unexpected deleted units: %+v", res.Deleted)
	}
	if res.Objects != 2 || res.Bytes != 7000 {
		t.Fatalf("unexpected totals: objects=%d bytes=%d", res.Objects, res.Bytes)
	}
	// 完整单元（有 manifest）必须原样保留（R-GC-04）。
	if _, ok := env.store.objs["vlogs/v1/c1/n1/20260101/manifest.json"]; !ok {
		t.Fatal("complete unit manifest must stay")
	}
	// fresh 前缀必须保留。
	if _, ok := env.store.objs["vlogs/v1/c1/n1/20260103/files/part.0"]; !ok {
		t.Fatal("fresh prefix must stay")
	}
}

func TestRunUnitFailureContinues(t *testing.T) {
	env := newTestEnv(t)
	// 两个 stale 单元：第一个单元的文件删除失败，第二个必须照常清理。
	env.addObj("vlogs/v1/c1/n1/20260101/files/part.0", 100, 48*time.Hour)
	env.addObj("vlogs/v1/c1/n1/20260102/files/part.0", 200, 48*time.Hour)
	env.store.deleteErrs["vlogs/v1/c1/n1/20260101/files/part.0"] = errors.New("injected delete failure")

	res, err := env.job.Run(context.Background(), true)
	if err == nil {
		t.Fatal("want aggregated error from failed unit")
	}
	if len(res.Deleted) != 1 || res.Deleted[0].Partition != "20260102" {
		t.Fatalf("other unit must still be deleted, got %+v", res.Deleted)
	}
}

// ---- DeleteUnit：先 manifest 后数据（R-GC-02）----

func TestDeleteUnitManifestFirst(t *testing.T) {
	env := newTestEnv(t)
	env.seed()
	mkey := "vlogs/v1/c1/n1/20260101/manifest.json"

	res, err := env.job.DeleteUnit(context.Background(), "20260101")
	if err != nil {
		t.Fatal(err)
	}
	if env.store.deleteCalls[0] != mkey {
		t.Fatalf("manifest must be deleted first, call order: %v", env.store.deleteCalls)
	}
	// 计数含 manifest：1(manifest) + 2(files) = 3。
	if res.Objects != 3 || res.Bytes != 3500 {
		t.Fatalf("unexpected counts: objects=%d bytes=%d", res.Objects, res.Bytes)
	}
	for _, k := range []string{mkey,
		"vlogs/v1/c1/n1/20260101/files/part.0",
		"vlogs/v1/c1/n1/20260101/files/part.1"} {
		if _, ok := env.store.objs[k]; ok {
			t.Fatalf("object %s must be deleted", k)
		}
	}
}

func TestDeleteUnitManifestDeleteFails(t *testing.T) {
	env := newTestEnv(t)
	env.seed()
	mkey := "vlogs/v1/c1/n1/20260101/manifest.json"
	env.store.deleteErrs[mkey] = errors.New("injected manifest delete failure")

	_, err := env.job.DeleteUnit(context.Background(), "20260101")
	if errs.ExitCode(err) != errs.ExitNetwork {
		t.Fatalf("want exit 3, got %v", err)
	}
	// manifest 删除失败 → 数据对象一个都不能动（先 manifest 后数据的强顺序）。
	if len(env.store.deleteCalls) != 1 {
		t.Fatalf("no data object may be deleted when manifest delete fails, calls: %v", env.store.deleteCalls)
	}
	if _, ok := env.store.objs["vlogs/v1/c1/n1/20260101/files/part.0"]; !ok {
		t.Fatal("data object must stay untouched")
	}
}

func TestDeleteUnitFileDeleteFails(t *testing.T) {
	env := newTestEnv(t)
	env.seed()
	failKey := "vlogs/v1/c1/n1/20260101/files/part.1"
	env.store.deleteErrs[failKey] = errors.New("injected file delete failure")

	_, err := env.job.DeleteUnit(context.Background(), "20260101")
	if errs.ExitCode(err) != errs.ExitNetwork {
		t.Fatalf("want exit 3, got %v", err)
	}
	// manifest 已删（安全状态：无 manifest 前缀），part.0 已删，part.1 残留。
	if _, ok := env.store.objs["vlogs/v1/c1/n1/20260101/manifest.json"]; ok {
		t.Fatal("manifest must already be deleted")
	}
	if _, ok := env.store.objs[failKey]; !ok {
		t.Fatal("failed object must remain")
	}
}

func TestDeleteUnitNoManifest(t *testing.T) {
	env := newTestEnv(t)
	env.seed()

	res, err := env.job.DeleteUnit(context.Background(), "20260102")
	if err != nil {
		t.Fatal(err)
	}
	if res.Objects != 2 || res.Bytes != 7000 {
		t.Fatalf("unexpected counts: objects=%d bytes=%d", res.Objects, res.Bytes)
	}
	if len(env.store.objs) != 4 { // complete 单元 3 个 + fresh 1 个
		t.Fatalf("unexpected remaining objects: %d", len(env.store.objs))
	}
}

func TestDeleteUnitInvalidPartition(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.job.DeleteUnit(context.Background(), "2026-01-01")
	if errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("want exit 2, got %v", err)
	}
}

func TestNewJobValidation(t *testing.T) {
	keys, err := s3store.NewKeyBuilder("vlogs", "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewJob(nil, keys, Options{}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("nil store must be usage error, got %v", err)
	}
	if _, err := NewJob(&fakeStore{}, nil, Options{}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("nil key builder must be usage error, got %v", err)
	}
	// StaleAfter ≤0 时兜底 24h。
	job, err := NewJob(&fakeStore{}, keys, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if job.opts.StaleAfter != 24*time.Hour {
		t.Fatalf("default stale-after = %v, want 24h", job.opts.StaleAfter)
	}
}
