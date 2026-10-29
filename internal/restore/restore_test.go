// restore 包测试：以故障注入覆盖回滚矩阵 §7.3 全部 5 行（AGENTS §5.3/§5.4）。
// VL 用 fakeVL（计数 + 按次消费错误），S3 用 fakeStore（内存实现），
// rename 与 diskFree 经 Job 非导出字段注入。禁止真实外网访问。
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/backupmeta"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/lock"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// ---------- fakes ----------

type fakeVL struct {
	detachErrs  []error // 按次消费
	attachErrs  []error // 按次消费
	detachCalls int
	attachCalls int
	smokeCalls  int
	smokeFound  bool
	smokeErr    error
}

func (f *fakeVL) Detach(ctx context.Context, partition string) error {
	f.detachCalls++
	if len(f.detachErrs) > 0 {
		err := f.detachErrs[0]
		f.detachErrs = f.detachErrs[1:]
		return err
	}
	return nil
}

func (f *fakeVL) Attach(ctx context.Context, partition string) error {
	f.attachCalls++
	if len(f.attachErrs) > 0 {
		err := f.attachErrs[0]
		f.attachErrs = f.attachErrs[1:]
		return err
	}
	return nil
}

func (f *fakeVL) SmokeQuery(ctx context.Context, partition string) (bool, error) {
	f.smokeCalls++
	return f.smokeFound, f.smokeErr
}

type fakeStore struct {
	objs        map[string][]byte
	headCalls   int
	getCalls    []string
	deleteCalls []string
}

func (f *fakeStore) Head(ctx context.Context, key string) (s3store.ObjectMeta, bool, error) {
	f.headCalls++
	data, ok := f.objs[key]
	if !ok {
		return s3store.ObjectMeta{}, false, nil
	}
	return s3store.ObjectMeta{Key: key, Size: int64(len(data))}, true, nil
}

func (f *fakeStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objs[key] = data
	return nil
}

func (f *fakeStore) Get(ctx context.Context, key string, w io.WriterAt) error {
	f.getCalls = append(f.getCalls, key)
	data, ok := f.objs[key]
	if !ok {
		return fmt.Errorf("object %s not found", key)
	}
	_, err := w.WriteAt(data, 0)
	return err
}

func (f *fakeStore) List(ctx context.Context, prefix string) ([]s3store.ObjectMeta, error) {
	return nil, nil
}

func (f *fakeStore) Delete(ctx context.Context, key string) error {
	f.deleteCalls = append(f.deleteCalls, key)
	delete(f.objs, key)
	return nil
}

// renameFaults 包装 os.Rename，按调用序注入失败（AGENTS §5.4 每失败点必测）。
type renameFaults struct {
	calls [][2]string
	fail  func(call int, src, dst string) bool
}

func (r *renameFaults) rename(oldname, newname string) error {
	r.calls = append(r.calls, [2]string{oldname, newname})
	if r.fail != nil && r.fail(len(r.calls), oldname, newname) {
		return fmt.Errorf("injected rename failure #%d (%s -> %s)", len(r.calls), oldname, newname)
	}
	return os.Rename(oldname, newname)
}

// ---------- 测试环境 ----------

type testEnv struct {
	t        *testing.T
	dataPath string
	job      *Job
	vl       *fakeVL
	store    *fakeStore
	rf       *renameFaults
	keys     *s3store.KeyBuilder
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dataPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataPath, "partitions"), 0o755); err != nil {
		t.Fatal(err)
	}
	vlf := &fakeVL{}
	stf := &fakeStore{objs: map[string][]byte{}}
	locks, err := lock.New(dataPath, "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	rf := &renameFaults{}
	job, err := NewJob(vlf, stf, locks, Options{
		Cluster:  "c1",
		Node:     "n1",
		DataPath: dataPath,
		Logger:   discardLogger,
	})
	if err != nil {
		t.Fatal(err)
	}
	job.diskFree = func(string) (uint64, error) { return 1 << 40, nil } // 1TiB，默认充足
	job.rename = rf.rename
	keys, err := s3store.NewKeyBuilder("", "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, dataPath: dataPath, job: job, vl: vlf, store: stf, rf: rf, keys: keys}
}

// putManifest 生成合法 manifest 与对应文件对象，提交到 fakeStore。
func (env *testEnv) putManifest(partition string, files map[string]string) *backupmeta.Manifest {
	env.t.Helper()
	m := &backupmeta.Manifest{
		FormatVersion: backupmeta.FormatVersion,
		Cluster:       "c1",
		Node:          "n1",
		Partition:     partition,
		CreatedAt:     time.Now().UTC(),
		SnapshotAt:    time.Now().UTC(),
		VLVersion:     "v1.21.0",
		ToolVersion:   "test",
	}
	for path, content := range files {
		sum := sha256.Sum256([]byte(content))
		m.Entries = append(m.Entries, backupmeta.Entry{
			Path:   path,
			Size:   int64(len(content)),
			SHA256: hex.EncodeToString(sum[:]),
		})
		key, err := env.keys.FileKey(partition, path)
		if err != nil {
			env.t.Fatal(err)
		}
		env.store.objs[key] = []byte(content)
	}
	m.FileCount = len(m.Entries)
	for _, e := range m.Entries {
		m.TotalBytes += e.Size
	}
	env.putRawManifest(partition, m)
	return m
}

// putRawManifest 把 manifest 以 JSON 提交到 fakeStore（供篡改场景使用）。
func (env *testEnv) putRawManifest(partition string, m *backupmeta.Manifest) {
	env.t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		env.t.Fatal(err)
	}
	mkey, err := env.keys.ManifestKey(partition)
	if err != nil {
		env.t.Fatal(err)
	}
	env.store.objs[mkey] = data
}

// writeTargetDir 在 partitions/ 下创建已有分区目录（旧数据）。
func (env *testEnv) writeTargetDir(partition string, files map[string]string) string {
	env.t.Helper()
	target := filepath.Join(env.dataPath, "partitions", partition)
	for name, content := range files {
		p := filepath.Join(target, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			env.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			env.t.Fatal(err)
		}
	}
	return target
}

// dirNames 返回 dataPath 下以 prefix 开头的目录名（replaced/orphan/临时目录检查）。
func (env *testEnv) dirNames(prefix string) []string {
	env.t.Helper()
	entries, err := os.ReadDir(env.dataPath)
	if err != nil {
		env.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func (env *testEnv) assertNoTempLeftover() {
	env.t.Helper()
	if left := env.dirNames(".restore-"); len(left) != 0 {
		env.t.Fatalf("temp dirs leaked: %v", left)
	}
}

func (env *testEnv) assertFileContent(path, want string) {
	env.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		env.t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		env.t.Fatalf("content of %s: got %q, want %q", path, string(data), want)
	}
}

// dayA 是窗口内的动态近日期（UTC 昨天），避免硬编码日期随真实时间推移落出保留窗口。
var dayA = time.Now().UTC().AddDate(0, 0, -1).Format("20060102")

// ---------- 主流程 ----------

func TestRestoreHappyPath(t *testing.T) {
	env := newTestEnv(t)
	content := `{"rows":42}`
	env.putManifest(dayA, map[string]string{"data/part.0": content})
	env.vl.smokeFound = true

	res, err := env.job.Run(context.Background(), dayA)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Attached {
		t.Error("expected attached")
	}
	if res.Smoke != SmokeOK {
		t.Errorf("smoke: got %q, want %q", res.Smoke, SmokeOK)
	}
	if res.Files != 1 || res.Bytes != int64(len(content)) {
		t.Errorf("result files/bytes: got %d/%d, want 1/%d", res.Files, res.Bytes, len(content))
	}
	if env.vl.detachCalls != 0 {
		t.Errorf("detach calls: got %d, want 0 (no local partition)", env.vl.detachCalls)
	}
	if env.vl.attachCalls != 1 {
		t.Errorf("attach calls: got %d, want 1", env.vl.attachCalls)
	}
	if res.Replaced {
		t.Error("expected replaced=false")
	}
	env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "part.0"), content)
	env.assertNoTempLeftover()
}

func TestRestoreExistingKeep(t *testing.T) {
	env := newTestEnv(t)
	env.writeTargetDir(dayA, map[string]string{"data/old.0": "old"})
	env.putManifest(dayA, map[string]string{"data/part.0": "new"})

	res, err := env.job.Run(context.Background(), dayA)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Replaced || res.ReplacedPath == "" {
		t.Fatalf("expected replaced with kept path, got %+v", res)
	}
	if env.vl.detachCalls != 1 || env.vl.attachCalls != 1 {
		t.Errorf("detach/attach calls: got %d/%d, want 1/1", env.vl.detachCalls, env.vl.attachCalls)
	}
	// 新数据落位 target；旧数据保留在 dataPath 层（不在 partitions/ 内，避免 VL 扫描）。
	env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "part.0"), "new")
	replaced := env.dirNames(dayA + ".replaced-")
	if len(replaced) != 1 {
		t.Fatalf("expected one replaced dir, got %v", replaced)
	}
	env.assertFileContent(filepath.Join(env.dataPath, replaced[0], "data", "old.0"), "old")
	env.assertNoTempLeftover()
}

func TestRestoreExistingDelete(t *testing.T) {
	env := newTestEnv(t)
	env.writeTargetDir(dayA, map[string]string{"data/old.0": "old"})
	env.putManifest(dayA, map[string]string{"data/part.0": "new"})
	env.job.opts.OnExisting = OnExistingDelete

	res, err := env.job.Run(context.Background(), dayA)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Replaced || res.ReplacedPath != "" {
		t.Errorf("expected replaced=true with empty path, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(env.dataPath, dayA+".replaced-0")); !os.IsNotExist(err) {
		t.Error("old dir should not be kept under delete policy")
	}
	env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "part.0"), "new")
	env.assertNoTempLeftover()
}

// 矩阵行①：下载/校验失败 → 删临时目录，分区未动（C4/R-RS-03）。
func TestRestoreDeepVerifyCorrupt(t *testing.T) {
	env := newTestEnv(t)
	m := env.putManifest(dayA, map[string]string{"data/part.0": "original"})
	// 篡改对象内容但保持长度一致，命中 sha256 分支而非 size 分支。
	key, err := env.keys.FileKey(dayA, "data/part.0")
	if err != nil {
		t.Fatal(err)
	}
	env.store.objs[key] = []byte("tampered") // 同长度 8 字节
	_ = m
	env.job.opts.Deep = true

	_, err = env.job.Run(context.Background(), dayA)
	if errs.ExitCode(err) != errs.ExitValidation {
		t.Fatalf("exit code: got %v (%v), want 5", err, errs.ExitCode(err))
	}
	if env.vl.detachCalls != 0 || env.vl.attachCalls != 0 {
		t.Errorf("partition must not be touched before verify: detach=%d attach=%d", env.vl.detachCalls, env.vl.attachCalls)
	}
	env.assertNoTempLeftover()
}

func TestRestoreSizeMismatch(t *testing.T) {
	env := newTestEnv(t)
	env.putManifest(dayA, map[string]string{"data/part.0": "short"})
	key, err := env.keys.FileKey(dayA, "data/part.0")
	if err != nil {
		t.Fatal(err)
	}
	env.store.objs[key] = []byte("much longer content than manifest says")

	_, err = env.job.Run(context.Background(), dayA)
	if errs.ExitCode(err) != errs.ExitValidation {
		t.Fatalf("exit code: got %v (%v), want 5", err, errs.ExitCode(err))
	}
	env.assertNoTempLeftover()
}

// R-RS-10：窗口边界（恰等于 vl_retention_days）允许；超窗拒绝且零 S3 请求。
func TestRestoreRetentionWindow(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	boundary := today.AddDate(0, 0, -181).Format("20060102")
	tooOld := today.AddDate(0, 0, -182).Format("20060102")
	recent := today.AddDate(0, 0, -1).Format("20060102")

	t.Run("boundary allowed", func(t *testing.T) {
		env := newTestEnv(t)
		env.putManifest(boundary, map[string]string{"data/part.0": "x"})
		if _, err := env.job.Run(context.Background(), boundary); err != nil {
			t.Fatalf("boundary partition must be allowed: %v", err)
		}
	})
	t.Run("beyond window rejected with zero s3 requests", func(t *testing.T) {
		env := newTestEnv(t)
		env.putManifest(tooOld, map[string]string{"data/part.0": "x"})
		_, err := env.job.Run(context.Background(), tooOld)
		if errs.ExitCode(err) != errs.ExitPrecondition {
			t.Fatalf("exit code: got %v (%v), want 6", err, errs.ExitCode(err))
		}
		if env.store.headCalls != 0 || len(env.store.getCalls) != 0 {
			t.Errorf("s3 requests must be zero: head=%d get=%d", env.store.headCalls, len(env.store.getCalls))
		}
	})
	t.Run("recent allowed", func(t *testing.T) {
		env := newTestEnv(t)
		env.putManifest(recent, map[string]string{"data/part.0": "x"})
		if _, err := env.job.Run(context.Background(), recent); err != nil {
			t.Fatalf("recent partition must be allowed: %v", err)
		}
	})
}

// R-RS-08：UTC 今天分区无 --force-today 退出码 2；有授权则走通。
func TestRestoreTodayProtection(t *testing.T) {
	today := time.Now().UTC().Format("20060102")

	t.Run("rejected", func(t *testing.T) {
		env := newTestEnv(t)
		env.putManifest(today, map[string]string{"data/part.0": "x"})
		_, err := env.job.Run(context.Background(), today)
		if errs.ExitCode(err) != errs.ExitUsage {
			t.Fatalf("exit code: got %v (%v), want 2", err, errs.ExitCode(err))
		}
		if env.store.headCalls != 0 {
			t.Error("no s3 request expected before today check passes")
		}
	})
	t.Run("force-today allowed", func(t *testing.T) {
		env := newTestEnv(t)
		env.putManifest(today, map[string]string{"data/part.0": "x"})
		env.job.opts.ForceToday = true
		if _, err := env.job.Run(context.Background(), today); err != nil {
			t.Fatalf("run: %v", err)
		}
	})
}

// R-RS-02：磁盘预检不足 → 退出码 6，且先于任何文件下载。
func TestRestoreDiskShortage(t *testing.T) {
	env := newTestEnv(t)
	env.putManifest(dayA, map[string]string{"data/part.0": "0123456789"})
	env.job.diskFree = func(string) (uint64, error) { return 0, nil }

	_, err := env.job.Run(context.Background(), dayA)
	if errs.ExitCode(err) != errs.ExitPrecondition {
		t.Fatalf("exit code: got %v (%v), want 6", err, errs.ExitCode(err))
	}
	if len(env.store.getCalls) != 1 {
		t.Errorf("only manifest download expected, got %d gets", len(env.store.getCalls))
	}
	if env.vl.detachCalls != 0 {
		t.Error("no detach expected before download")
	}
	env.assertNoTempLeftover()
}

// C3：无 manifest 的前缀禁止恢复（退出码 4）。
func TestRestoreNoManifest(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.job.Run(context.Background(), dayA)
	if errs.ExitCode(err) != errs.ExitNotFound {
		t.Fatalf("exit code: got %v (%v), want 4", err, errs.ExitCode(err))
	}
}

// R-MF-04：身份比对不一致 → 退出码 5。
func TestRestoreIdentityMismatch(t *testing.T) {
	env := newTestEnv(t)
	m := env.putManifest(dayA, map[string]string{"data/part.0": "x"})
	m.Node = "other-node"
	env.putRawManifest(dayA, m)

	_, err := env.job.Run(context.Background(), dayA)
	if errs.ExitCode(err) != errs.ExitValidation {
		t.Fatalf("exit code: got %v (%v), want 5", err, errs.ExitCode(err))
	}
}

// R-MF-04：cluster/node 不一致 + --allow-node-mismatch → 放行且摘要标注跨节点恢复。
func TestRestoreAllowNodeMismatch(t *testing.T) {
	env := newTestEnv(t)
	m := env.putManifest(dayA, map[string]string{"data/part.0": "x"})
	m.Node = "other-node"
	env.putRawManifest(dayA, m)
	env.job.opts.AllowNodeMismatch = true

	res, err := env.job.Run(context.Background(), dayA)
	if err != nil {
		t.Fatalf("run with allow-node-mismatch: %v", err)
	}
	if !res.CrossNode {
		t.Error("result must be marked as cross-node restore")
	}
}

// C8：partition 不一致即使开启 --allow-node-mismatch 也永远拒绝（退出码 5）。
func TestRestorePartitionMismatchAlwaysRejected(t *testing.T) {
	env := newTestEnv(t)
	m := env.putManifest(dayA, map[string]string{"data/part.0": "x"})
	m.Partition = "2026-01-01"
	env.putRawManifest(dayA, m)
	env.job.opts.AllowNodeMismatch = true

	_, err := env.job.Run(context.Background(), dayA)
	if errs.ExitCode(err) != errs.ExitValidation {
		t.Fatalf("exit code: got %v (%v), want 5", err, errs.ExitCode(err))
	}
}

// R-MF-03：manifest 版本过新 → 退出码 5。
func TestRestoreManifestTooNew(t *testing.T) {
	env := newTestEnv(t)
	m := env.putManifest(dayA, map[string]string{"data/part.0": "x"})
	m.FormatVersion = backupmeta.FormatVersion + 1
	env.putRawManifest(dayA, m)

	_, err := env.job.Run(context.Background(), dayA)
	if errs.ExitCode(err) != errs.ExitValidation {
		t.Fatalf("exit code: got %v (%v), want 5", err, errs.ExitCode(err))
	}
	if !errors.Is(err, backupmeta.ErrVersionTooNew) {
		t.Errorf("error should wrap ErrVersionTooNew: %v", err)
	}
}

func TestRestoreManifestBrokenJSON(t *testing.T) {
	env := newTestEnv(t)
	mkey, err := env.keys.ManifestKey(dayA)
	if err != nil {
		t.Fatal(err)
	}
	env.store.objs[mkey] = []byte("{broken")

	_, err = env.job.Run(context.Background(), dayA)
	if errs.ExitCode(err) != errs.ExitValidation {
		t.Fatalf("exit code: got %v (%v), want 5", err, errs.ExitCode(err))
	}
}

// detach 失败：本地目录未动，尽力 attach 回 present，退出码 3。
func TestRestoreDetachFailure(t *testing.T) {
	env := newTestEnv(t)
	env.writeTargetDir(dayA, map[string]string{"data/old.0": "old"})
	env.putManifest(dayA, map[string]string{"data/part.0": "new"})
	env.vl.detachErrs = []error{errors.New("vl unreachable")}

	_, err := env.job.Run(context.Background(), dayA)
	if errs.ExitCode(err) != errs.ExitNetwork {
		t.Fatalf("exit code: got %v (%v), want 3", err, errs.ExitCode(err))
	}
	if env.vl.attachCalls != 1 {
		t.Errorf("best-effort attach calls: got %d, want 1", env.vl.attachCalls)
	}
	env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "old.0"), "old")
	env.assertNoTempLeftover()
}

// 矩阵行②：detach 后旧目录 rename 失败 → 回滚 attach 原目录，旧目录原位。
func TestRollbackOldRenameFailure(t *testing.T) {
	env := newTestEnv(t)
	env.writeTargetDir(dayA, map[string]string{"data/old.0": "old"})
	env.putManifest(dayA, map[string]string{"data/part.0": "new"})
	env.rf.fail = func(call int, src, dst string) bool { return call == 1 }

	_, err := env.job.Run(context.Background(), dayA)
	if err == nil {
		t.Fatal("expected error")
	}
	if errs.ExitCode(err) != errs.ExitGeneral {
		t.Errorf("exit code: got %d, want 1", errs.ExitCode(err))
	}
	if env.vl.attachCalls != 1 {
		t.Errorf("rollback attach calls: got %d, want 1", env.vl.attachCalls)
	}
	env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "old.0"), "old")
	if left := env.dirNames(dayA + ".replaced-"); len(left) != 0 {
		t.Errorf("no replaced dir expected, got %v", left)
	}
	env.assertNoTempLeftover()
}

// 矩阵行③：新目录 rename 失败 → 旧目录改回原名 → attach。
func TestRollbackNewRenameFailure(t *testing.T) {
	env := newTestEnv(t)
	env.writeTargetDir(dayA, map[string]string{"data/old.0": "old"})
	env.putManifest(dayA, map[string]string{"data/part.0": "new"})
	// call 1 = 旧目录 rename（成功），call 2 = tmp → target（失败）。
	env.rf.fail = func(call int, src, dst string) bool { return call == 2 }

	_, err := env.job.Run(context.Background(), dayA)
	if err == nil {
		t.Fatal("expected error")
	}
	if env.vl.attachCalls != 1 {
		t.Errorf("rollback attach calls: got %d, want 1", env.vl.attachCalls)
	}
	env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "old.0"), "old")
	if left := env.dirNames(dayA + ".replaced-"); len(left) != 0 {
		t.Errorf("replaced dir should be renamed back, got %v", left)
	}
	env.assertNoTempLeftover()
}

// 矩阵行④：attach 重试一次仍失败 → 停现场 + 人工命令，退出码 1，目录一致。
func TestAttachRetryThenManual(t *testing.T) {
	env := newTestEnv(t)
	env.writeTargetDir(dayA, map[string]string{"data/old.0": "old"})
	env.putManifest(dayA, map[string]string{"data/part.0": "new"})
	env.vl.attachErrs = []error{errors.New("boom 1"), errors.New("boom 2")}

	res, err := env.job.Run(context.Background(), dayA)
	if err == nil {
		t.Fatal("expected error")
	}
	if errs.ExitCode(err) != errs.ExitGeneral {
		t.Errorf("exit code: got %d, want 1", errs.ExitCode(err))
	}
	if env.vl.attachCalls != 2 {
		t.Errorf("attach calls: got %d, want 2 (retry once)", env.vl.attachCalls)
	}
	// 新数据已落位、旧数据保留在 replaced（不做目录级回滚）。
	env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "part.0"), "new")
	if left := env.dirNames(dayA + ".replaced-"); len(left) != 1 {
		t.Errorf("replaced dir must be kept for manual recovery, got %v", left)
	}
	if !strings.Contains(err.Error(), "manual") || !strings.Contains(err.Error(), env.dataPath) {
		t.Errorf("error must contain manual instructions with absolute paths: %v", err)
	}
	if res.Attached {
		t.Error("attached must be false")
	}
}

// 矩阵行⑤：回滚自身失败 → 终止 + 现场状态 + 人工命令。
func TestRollbackItselfFails(t *testing.T) {
	env := newTestEnv(t)
	env.writeTargetDir(dayA, map[string]string{"data/old.0": "old"})
	env.putManifest(dayA, map[string]string{"data/part.0": "new"})
	// call 2 = 主流程 tmp → target 失败；rollback 内 call 3 = 旧目录回原位也失败。
	env.rf.fail = func(call int, src, dst string) bool { return call >= 2 }

	_, err := env.job.Run(context.Background(), dayA)
	if err == nil {
		t.Fatal("expected error")
	}
	if errs.ExitCode(err) != errs.ExitGeneral {
		t.Errorf("exit code: got %d, want 1", errs.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "rollback incomplete") || !strings.Contains(err.Error(), "manual") {
		t.Errorf("error must state rollback incompleteness and manual action: %v", err)
	}
}

// R-RS-07：冒烟查询三态均不改变退出码。
func TestSmokeStates(t *testing.T) {
	cases := []struct {
		name      string
		found     bool
		smokeErr  error
		wantSmoke string
	}{
		{"ok", true, nil, SmokeOK},
		{"empty", false, nil, SmokeEmpty},
		{"failed", false, errors.New("query failed"), SmokeFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.putManifest(dayA, map[string]string{"data/part.0": "x"})
			env.vl.smokeFound, env.vl.smokeErr = tc.found, tc.smokeErr

			res, err := env.job.Run(context.Background(), dayA)
			if err != nil {
				t.Fatalf("smoke state must not change exit code: %v", err)
			}
			if res.Smoke != tc.wantSmoke {
				t.Errorf("smoke: got %q, want %q", res.Smoke, tc.wantSmoke)
			}
		})
	}
}

// R-RS-09：拒绝符号链接（partitions 目录本身或目标分区）。
func TestPathSafetySymlink(t *testing.T) {
	t.Run("partitions is symlink", func(t *testing.T) {
		env := newTestEnv(t)
		external := filepath.Join(env.dataPath, "external-parts")
		if err := os.MkdirAll(external, 0o755); err != nil {
			t.Fatal(err)
		}
		parts := filepath.Join(env.dataPath, "partitions")
		if err := os.Remove(parts); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, parts); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		_, err := env.job.Run(context.Background(), dayA)
		if errs.ExitCode(err) != errs.ExitUsage {
			t.Fatalf("exit code: got %v (%v), want 2", err, errs.ExitCode(err))
		}
	})
	t.Run("target is symlink", func(t *testing.T) {
		env := newTestEnv(t)
		external := filepath.Join(env.dataPath, "external-part")
		if err := os.MkdirAll(external, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(env.dataPath, "partitions", dayA)
		if err := os.Symlink(external, target); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		env.putManifest(dayA, map[string]string{"data/part.0": "x"})
		_, err := env.job.Run(context.Background(), dayA)
		if errs.ExitCode(err) != errs.ExitUsage {
			t.Fatalf("exit code: got %v (%v), want 2", err, errs.ExitCode(err))
		}
		if env.vl.detachCalls != 0 {
			t.Error("symlink target must not be detached")
		}
	})
}

func TestInvalidPartition(t *testing.T) {
	for _, p := range []string{"2026-01-01", "abc", "20261301", "2026010", "202601011"} {
		t.Run(p, func(t *testing.T) {
			env := newTestEnv(t)
			_, err := env.job.Run(context.Background(), p)
			if errs.ExitCode(err) != errs.ExitUsage {
				t.Fatalf("exit code: got %v (%v), want 2", err, errs.ExitCode(err))
			}
			if env.store.headCalls != 0 {
				t.Error("no s3 request expected for invalid partition")
			}
		})
	}
}

// ---------- rollbackToPresent 单元（矩阵行②③④⑤） ----------

func TestRollbackUnit(t *testing.T) {
	type fixture struct {
		oldMoved    bool
		oldGone     bool
		newPath     bool
		detached    bool
		writeOld    bool // 在 oldPath 预置旧目录
		writeNew    bool // 在 target 预置新目录（newPath 已落位）
		writeTmp    bool // 在 tmpDir 预置下载目录
		attachFails bool
	}
	cases := []struct {
		name  string
		fx    fixture
		check func(t *testing.T, env *testEnv)
	}{
		{
			// 行②：旧目录在 replaced → 改回原名 → attach。
			name: "old moved back",
			fx:   fixture{detached: true, oldMoved: true, writeOld: true},
			check: func(t *testing.T, env *testEnv) {
				env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "old.0"), "old")
				if left := env.dirNames(dayA + ".replaced-"); len(left) != 0 {
					t.Errorf("replaced dir should be gone: %v", left)
				}
				if env.vl.attachCalls != 1 {
					t.Errorf("attach calls: %d", env.vl.attachCalls)
				}
			},
		},
		{
			// 行③b：旧目录已删、下载目录完整 → 落位 → attach（分区不缺失）。
			name: "tmp moved into place",
			fx:   fixture{detached: true, oldGone: true, writeTmp: true},
			check: func(t *testing.T, env *testEnv) {
				env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "part.0"), "new")
				if left := env.dirNames(".restore-"); len(left) != 0 {
					t.Errorf("tmp dir should be gone: %v", left)
				}
				if env.vl.attachCalls != 1 {
					t.Errorf("attach calls: %d", env.vl.attachCalls)
				}
			},
		},
		{
			// 行③a：新目录已落位且旧目录在 replaced → 新目录挪孤儿位，旧目录回原位 → attach。
			name: "new orphaned and old restored",
			fx:   fixture{detached: true, oldMoved: true, newPath: true, writeOld: true, writeNew: true},
			check: func(t *testing.T, env *testEnv) {
				env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "old.0"), "old")
				orphans := env.dirNames(dayA + ".orphan-")
				if len(orphans) != 1 {
					t.Fatalf("expected one orphan dir, got %v", orphans)
				}
				env.assertFileContent(filepath.Join(env.dataPath, orphans[0], "data", "part.0"), "new")
			},
		},
		{
			// 新目录已在位且无旧目录 → 直接 attach。
			name: "new in place attach only",
			fx:   fixture{detached: true, oldGone: true, newPath: true, writeNew: true},
			check: func(t *testing.T, env *testEnv) {
				env.assertFileContent(filepath.Join(env.dataPath, "partitions", dayA, "data", "part.0"), "new")
				if env.vl.attachCalls != 1 {
					t.Errorf("attach calls: %d", env.vl.attachCalls)
				}
			},
		},
		{
			// 行⑤：回滚 attach 失败 → 返回错误（finishRollback 负责终态报文）。
			name: "rollback attach fails",
			fx:   fixture{detached: true, writeOld: true, attachFails: true},
			check: func(t *testing.T, env *testEnv) {
				if env.vl.attachCalls != 1 {
					t.Errorf("attach calls: %d", env.vl.attachCalls)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			target := filepath.Join(env.dataPath, "partitions", dayA)
			var oldPath, tmpDir string
			if tc.fx.writeOld {
				oldPath = filepath.Join(env.dataPath, dayA+".replaced-1700000000")
				writeTree(t, oldPath, map[string]string{"data/old.0": "old"})
			}
			if tc.fx.writeNew {
				writeTree(t, target, map[string]string{"data/part.0": "new"})
			}
			if tc.fx.writeTmp {
				tmpDir = filepath.Join(env.dataPath, ".restore-"+dayA+"-tmp")
				writeTree(t, tmpDir, map[string]string{"data/part.0": "new"})
			}
			if tc.fx.attachFails {
				env.vl.attachErrs = []error{errors.New("vl down"), errors.New("vl down"), errors.New("vl down")}
			}
			st := &restoreState{
				partition: dayA,
				dataPath:  env.dataPath,
				target:    target,
				tmpDir:    tmpDir,
				detached:  tc.fx.detached,
				oldMoved:  tc.fx.oldMoved,
				oldGone:   tc.fx.oldGone,
				newPath:   tc.fx.newPath,
				oldPath:   oldPath,
			}
			err := env.job.rollbackToPresent(context.Background(), st)
			if tc.fx.attachFails {
				if err == nil {
					t.Fatal("expected error when rollback attach fails")
				}
			} else if err != nil {
				t.Fatalf("rollback: %v", err)
			}
			tc.check(t, env)
		})
	}
}

// rollbackToPresent 可重入：已回滚完成的状态再次调用是 no-op。
func TestRollbackUnitReentrant(t *testing.T) {
	env := newTestEnv(t)
	st := &restoreState{
		partition: dayA,
		dataPath:  env.dataPath,
		target:    filepath.Join(env.dataPath, "partitions", dayA),
	}
	if err := env.job.rollbackToPresent(context.Background(), st); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := env.job.rollbackToPresent(context.Background(), st); err != nil {
		t.Fatalf("second call must be no-op: %v", err)
	}
	if env.vl.attachCalls != 0 {
		t.Errorf("no attach expected for untouched state: %d", env.vl.attachCalls)
	}
}

// sliceWriter：乱序偏移写入落位正确。
func TestSliceWriter(t *testing.T) {
	w := &sliceWriter{}
	if _, err := w.WriteAt([]byte("world"), 6); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("hello "), 0); err != nil {
		t.Fatal(err)
	}
	if string(w.buf) != "hello world" {
		t.Fatalf("got %q", string(w.buf))
	}
	if _, err := w.WriteAt([]byte("x"), -1); err == nil {
		t.Fatal("negative offset must fail")
	}
}

// writeTree 在 dir 下写一组文件（辅助 RollbackUnit 构造现场）。
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
