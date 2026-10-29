package purge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/lock"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
)

// 测试日期取动态「昨天」，避免硬编码日期随真实时间落入今天/保留窗口等意外区间（C8）。
var (
	dayA   = time.Now().UTC().AddDate(0, 0, -1).Format("20060102")
	todayP = time.Now().UTC().Format("20060102")
)

// ---- 故障注入基建（与 internal/restore 同模式，AGENTS §5.4）----

type fakeVL struct {
	detachErrs  []error
	attachErrs  []error
	detachCalls int
	attachCalls int
}

func (f *fakeVL) Detach(_ context.Context, _ string) error {
	i := f.detachCalls
	f.detachCalls++
	if i < len(f.detachErrs) {
		return f.detachErrs[i]
	}
	return nil
}

func (f *fakeVL) Attach(_ context.Context, _ string) error {
	i := f.attachCalls
	f.attachCalls++
	if i < len(f.attachErrs) {
		return f.attachErrs[i]
	}
	return nil
}

type fakeBackup struct {
	manifests map[string]bool
	headErr   error
	headKeys  []string
}

func (f *fakeBackup) Head(_ context.Context, key string) (s3store.ObjectMeta, bool, error) {
	if f.headErr != nil {
		return s3store.ObjectMeta{}, false, f.headErr
	}
	f.headKeys = append(f.headKeys, key)
	return s3store.ObjectMeta{Key: key}, f.manifests[key], nil
}

type testEnv struct {
	t    *testing.T
	root string // VL --storageDataPath
	keys *s3store.KeyBuilder
	job  *Job
	vl   *fakeVL
	bk   *fakeBackup

	renameCalls [][2]string
	renameFail  func(call int, old, new string) bool

	removeCalls []string
	removeFail  func(call int, path string) bool
	afterRemove func(path string) // 每次 RemoveAll 成功后的钩子（模拟并发重建等）
}

func newTestEnv(t *testing.T, mod func(*Options)) *testEnv {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "partitions"), 0o755); err != nil {
		t.Fatal(err)
	}
	vl := &fakeVL{}
	bk := &fakeBackup{manifests: map[string]bool{}}
	locks, err := lock.New(root, "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Cluster:  "c1",
		Node:     "n1",
		S3Prefix: "vlogs",
		DataPath: root,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mod != nil {
		mod(&opts)
	}
	keys, err := s3store.NewKeyBuilder("vlogs", "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := NewJob(vl, bk, locks, opts)
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{t: t, root: root, keys: keys, job: job, vl: vl, bk: bk}
	job.rename = env.doRename
	job.remove = env.doRemove
	return env
}

func (e *testEnv) doRename(old, new string) error {
	e.renameCalls = append(e.renameCalls, [2]string{old, new})
	if e.renameFail != nil && e.renameFail(len(e.renameCalls), old, new) {
		return errors.New("injected rename failure")
	}
	return os.Rename(old, new)
}

func (e *testEnv) doRemove(path string) error {
	e.removeCalls = append(e.removeCalls, path)
	if e.removeFail != nil && e.removeFail(len(e.removeCalls), path) {
		return errors.New("injected remove failure")
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if e.afterRemove != nil {
		e.afterRemove(path)
	}
	return nil
}

func (e *testEnv) target(partition string) string {
	return filepath.Join(e.root, "partitions", partition)
}

func (e *testEnv) writeTargetDir(partition string, files int) {
	e.t.Helper()
	dir := e.target(partition)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i < files; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("part.%05d", i)),
			[]byte(strings.Repeat("x", 100+i)), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *testEnv) putManifest(partition string) {
	e.t.Helper()
	mkey, err := e.keys.ManifestKey(partition)
	if err != nil {
		e.t.Fatal(err)
	}
	e.bk.manifests[mkey] = true
}

func (e *testEnv) deletingEntries() []string {
	e.t.Helper()
	matches, err := filepath.Glob(filepath.Join(e.root, ".deleting-*"))
	if err != nil {
		e.t.Fatal(err)
	}
	return matches
}

func (e *testEnv) partitionsEntries() []string {
	e.t.Helper()
	matches, err := filepath.Glob(filepath.Join(e.root, "partitions", "*"))
	if err != nil {
		e.t.Fatal(err)
	}
	return matches
}

func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("want exit code %d, got nil error", want)
	}
	if got := errs.ExitCode(err); got != want {
		t.Fatalf("want exit code %d, got %d: %v", want, got, err)
	}
}

// ---- Run 主流程 ----

func TestRunHappyPath(t *testing.T) {
	env := newTestEnv(t, nil)
	env.writeTargetDir(dayA, 3)
	env.putManifest(dayA)

	res, err := env.job.Run(context.Background(), dayA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Partition != dayA || res.Path != env.target(dayA) {
		t.Fatalf("unexpected result path %q / partition %q", res.Path, res.Partition)
	}
	if res.Files != 3 || res.Bytes <= 0 {
		t.Fatalf("unexpected scale: files=%d bytes=%d", res.Files, res.Bytes)
	}
	if env.vl.detachCalls != 1 {
		t.Fatalf("want exactly 1 detach, got %d", env.vl.detachCalls)
	}
	// R-DEL-06：partitions/ 下不留任何东西，.deleting-* 也应清理干净。
	if got := env.partitionsEntries(); len(got) != 0 {
		t.Fatalf("partitions/ not empty: %v", got)
	}
	if got := env.deletingEntries(); len(got) != 0 {
		t.Fatalf(".deleting-* leftovers: %v", got)
	}
}

func TestRunPartitionMissing(t *testing.T) {
	env := newTestEnv(t, nil)
	_, err := env.job.Run(context.Background(), dayA)
	assertExitCode(t, err, errs.ExitNotFound)
	if env.vl.detachCalls != 0 {
		t.Fatalf("must not detach a missing partition, got %d calls", env.vl.detachCalls)
	}
}

func TestRunTodayPartition(t *testing.T) {
	t.Run("without force today rejected", func(t *testing.T) {
		env := newTestEnv(t, nil)
		env.writeTargetDir(todayP, 1)
		env.putManifest(todayP)
		_, err := env.job.Run(context.Background(), todayP)
		assertExitCode(t, err, errs.ExitPrecondition)
		if env.vl.detachCalls != 0 {
			t.Fatalf("must not detach today partition without authorization, got %d", env.vl.detachCalls)
		}
	})
	t.Run("with force today proceeds", func(t *testing.T) {
		env := newTestEnv(t, func(o *Options) { o.ForceToday = true })
		env.writeTargetDir(todayP, 1)
		env.putManifest(todayP)
		res, err := env.job.Run(context.Background(), todayP)
		if err != nil {
			t.Fatal(err)
		}
		if res.Partition != todayP {
			t.Fatalf("unexpected partition %q", res.Partition)
		}
	})
}

func TestRunNoBackupGuard(t *testing.T) {
	t.Run("without force no backup rejected", func(t *testing.T) {
		env := newTestEnv(t, nil)
		env.writeTargetDir(dayA, 2) // 无 manifest
		_, err := env.job.Run(context.Background(), dayA)
		assertExitCode(t, err, errs.ExitPrecondition)
		if env.vl.detachCalls != 0 {
			t.Fatalf("must not detach without backup guard pass, got %d", env.vl.detachCalls)
		}
		if _, statErr := os.Lstat(env.target(dayA)); statErr != nil {
			t.Fatalf("target dir must stay untouched: %v", statErr)
		}
	})
	t.Run("with force no backup proceeds", func(t *testing.T) {
		env := newTestEnv(t, func(o *Options) { o.ForceNoBackup = true })
		env.writeTargetDir(dayA, 2)
		res, err := env.job.Run(context.Background(), dayA)
		if err != nil {
			t.Fatal(err)
		}
		if res.Files != 2 {
			t.Fatalf("unexpected files %d", res.Files)
		}
	})
}

func TestRunInvalidPartition(t *testing.T) {
	cases := map[string]string{
		"empty":     "",
		"too short": "2026101",
		"too long":  "202610270",
		"not digit": "abcdefgh",
		"bad date":  "20261301",
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, nil)
			_, err := env.job.Run(context.Background(), p)
			assertExitCode(t, err, errs.ExitUsage)
			if env.vl.detachCalls != 0 || len(env.renameCalls) != 0 {
				t.Fatal("no filesystem action allowed for invalid partition name")
			}
		})
	}
}

// ---- 失败表逐行故障注入（设计文档 §7.5，AGENTS §5.4）----

// 失败点 1：detach 失败 → 退 3，目录未动，不回滚。
func TestRunDetachFailure(t *testing.T) {
	env := newTestEnv(t, nil)
	env.writeTargetDir(dayA, 2)
	env.putManifest(dayA)
	env.vl.detachErrs = []error{errors.New("endpoint down")}

	_, err := env.job.Run(context.Background(), dayA)
	assertExitCode(t, err, errs.ExitNetwork)
	if _, statErr := os.Lstat(env.target(dayA)); statErr != nil {
		t.Fatalf("target dir must stay untouched after detach failure: %v", statErr)
	}
	if len(env.renameCalls) != 0 {
		t.Fatal("must not touch dir when detach failed")
	}
}

// 失败点 2：detach 成功、rename 失败 → 退 1 且必须 attach 回来（R-DEL-09）。
func TestRunRenameFailure(t *testing.T) {
	env := newTestEnv(t, nil)
	env.writeTargetDir(dayA, 2)
	env.putManifest(dayA)
	env.renameFail = func(call int, _, _ string) bool { return call == 1 }

	_, err := env.job.Run(context.Background(), dayA)
	assertExitCode(t, err, errs.ExitGeneral)
	if env.vl.attachCalls != 1 {
		t.Fatalf("partition must be attached back exactly once, got %d", env.vl.attachCalls)
	}
	if _, statErr := os.Lstat(env.target(dayA)); statErr != nil {
		t.Fatalf("target dir must still exist (rename failed before moving): %v", statErr)
	}
}

// 失败点 3：删除中失败 → 退 1，不回滚，错误含残留路径与人工命令。
func TestRunRemoveFailure(t *testing.T) {
	env := newTestEnv(t, nil)
	env.writeTargetDir(dayA, 2)
	env.putManifest(dayA)
	env.removeFail = func(call int, _ string) bool { return call == 1 }

	_, err := env.job.Run(context.Background(), dayA)
	assertExitCode(t, err, errs.ExitGeneral)
	if env.vl.attachCalls != 0 {
		t.Fatalf("must not attach back after deletion started, got %d", env.vl.attachCalls)
	}
	deleting := env.deletingEntries()
	if len(deleting) != 1 {
		t.Fatalf("want exactly 1 .deleting-* leftover, got %v", deleting)
	}
	for _, want := range []string{"rm -rf", deleting[0]} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must contain %q: %v", want, err)
		}
	}
	// partitions/ 必须干净（残留只在 .deleting-*，不在 partitions/ 下）。
	if got := env.partitionsEntries(); len(got) != 0 {
		t.Fatalf("partitions/ must be clean, got %v", got)
	}
}

// 失败点 4：复查发现分区目录仍在 → 退 1 + 绝对路径人工指引。
func TestRunRecheckFindsTarget(t *testing.T) {
	env := newTestEnv(t, nil)
	env.writeTargetDir(dayA, 2)
	env.putManifest(dayA)
	target := env.target(dayA)
	// 模拟并发重建：RemoveAll 成功后 target 目录重新出现。
	env.afterRemove = func(string) {
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	_, err := env.job.Run(context.Background(), dayA)
	assertExitCode(t, err, errs.ExitGeneral)
	if !strings.Contains(err.Error(), target) {
		t.Fatalf("error must contain absolute path %q: %v", target, err)
	}
}

// ---- Guard 护栏表驱动（R-DEL-07/08）----

func TestGuard(t *testing.T) {
	cases := []struct {
		name         string
		info         Info
		forceNoBakup bool
		forceToday   bool
		wantCode     int
	}{
		{name: "missing partition", info: Info{Partition: dayA, Path: "/x/20261027"},
			wantCode: errs.ExitNotFound},
		{name: "today without force", info: Info{Partition: todayP, Path: "/x", Exists: true, HasBackup: true, IsToday: true},
			wantCode: errs.ExitPrecondition},
		{name: "today with force", info: Info{Partition: todayP, Path: "/x", Exists: true, HasBackup: true, IsToday: true},
			forceToday: true, wantCode: -1},
		{name: "no backup without force", info: Info{Partition: dayA, Path: "/x", Exists: true},
			wantCode: errs.ExitPrecondition},
		{name: "no backup with force", info: Info{Partition: dayA, Path: "/x", Exists: true},
			forceNoBakup: true, wantCode: -1},
		{name: "happy", info: Info{Partition: dayA, Path: "/x", Exists: true, HasBackup: true},
			wantCode: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Guard(tc.info, tc.forceNoBakup, tc.forceToday)
			if tc.wantCode == -1 {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			assertExitCode(t, err, tc.wantCode)
		})
	}
}

// ---- Inspect 只读收集（R-DEL-02 数据源）----

func TestInspectCollectsFields(t *testing.T) {
	env := newTestEnv(t, nil)
	env.writeTargetDir(dayA, 2)

	info, err := env.job.Inspect(context.Background(), dayA)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Exists || info.Files != 2 || info.Bytes <= 0 {
		t.Fatalf("unexpected scale fields: %+v", info)
	}
	if info.IsToday {
		t.Fatal("yesterday must not be flagged as today")
	}
	wantKey, err := env.keys.ManifestKey(dayA)
	if err != nil {
		t.Fatal(err)
	}
	if info.BackupKey != wantKey {
		t.Fatalf("BackupKey = %q, want %q", info.BackupKey, wantKey)
	}
	wantUnit, err := env.keys.UnitPrefix(dayA)
	if err != nil {
		t.Fatal(err)
	}
	if info.UnitPrefix != wantUnit {
		t.Fatalf("UnitPrefix = %q, want %q", info.UnitPrefix, wantUnit)
	}
	if info.HasBackup {
		t.Fatal("no manifest put, HasBackup must be false")
	}
	if len(env.bk.headKeys) != 1 || env.bk.headKeys[0] != wantKey {
		t.Fatalf("Head must be called once with manifest key, got %v", env.bk.headKeys)
	}
}

func TestInspectSymlinkRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	link := env.target("20260909")
	real := filepath.Join(env.root, "elsewhere")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not available on this platform: %v", err)
	}
	_, err := env.job.Inspect(context.Background(), "20260909")
	assertExitCode(t, err, errs.ExitUsage)
}

func TestInspectHeadFailure(t *testing.T) {
	env := newTestEnv(t, nil)
	env.writeTargetDir(dayA, 1)
	env.bk.headErr = errors.New("network split")
	_, err := env.job.Inspect(context.Background(), dayA)
	assertExitCode(t, err, errs.ExitNetwork)
}

// ---- 确认提示（R-DEL-02 六字段）----

func TestRenderConfirmHasBackup(t *testing.T) {
	var buf bytes.Buffer
	RenderConfirm(&buf, Info{
		Partition:  "20261027",
		Path:       "/var/lib/victoria-logs/partitions/20261027",
		Exists:     true,
		Files:      12438,
		Bytes:      62599148339,
		HasBackup:  true,
		BackupKey:  "vlogs/v1/prod/node0/20261027/manifest.json",
		UnitPrefix: "vlogs/v1/prod/node0/20261027/",
	})
	out := buf.String()
	for _, want := range []string{
		"即将删除本地分区数据，此操作不可恢复",
		"分区        : 2026-10-27 (UTC)",
		"目录        : /var/lib/victoria-logs/partitions/20261027",
		"规模        : 12438 个文件 / 58.3 GiB",
		"在线备份    : 有（已提交 vlogs/v1/prod/node0/20261027/manifest.json）",
		"影响        : 该日日志将立即不可查询；其他日期不受影响；进程不重启",
		"可恢复性    : 已有已提交备份",
		"restore 2026-10-27",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("confirm text must contain %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "警告") {
		t.Fatalf("non-today info must not print today warning:\n%s", out)
	}
}

func TestRenderConfirmNoBackup(t *testing.T) {
	var buf bytes.Buffer
	RenderConfirm(&buf, Info{
		Partition:  "20261027",
		Path:       "/data/partitions/20261027",
		Files:      3,
		Bytes:      2048,
		UnitPrefix: "vlogs/v1/c1/n1/20261027/",
	})
	out := buf.String()
	for _, want := range []string{
		"在线备份    : 无（vlogs/v1/c1/n1/20261027/ 无 manifest）",
		"可恢复性    : 无备份，删除后无法找回",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("confirm text must contain %q\n---\n%s", want, out)
		}
	}
}

func TestRenderConfirmTodayWarning(t *testing.T) {
	var buf bytes.Buffer
	RenderConfirm(&buf, Info{Partition: "20261028", Path: "/x", Exists: true, IsToday: true, HasBackup: true})
	if !strings.Contains(buf.String(), "警告        : 该分区是 UTC 今天") {
		t.Fatalf("today info must print warning:\n%s", buf.String())
	}
}

// ---- 字节格式化 ----

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{62599148339, "58.3 GiB"},
		{3 * 1024 * 1024 * 1024 * 1024, "3.0 TiB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---- 残留自检（R-DEL-11）----

func TestCleanupDeleting(t *testing.T) {
	root := t.TempDir()
	left1 := filepath.Join(root, ".deleting-20261027-111")
	left2 := filepath.Join(root, ".deleting-20261028-222")
	keep := filepath.Join(root, "partitions")
	for _, d := range []string{left1, left2, keep} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(left1, "part.0"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := CleanupDeleting(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 removed, got %d", n)
	}
	for _, p := range []string{left1, left2} {
		if _, serr := os.Lstat(p); !os.IsNotExist(serr) {
			t.Fatalf("leftover %s must be removed (stat err %v)", p, serr)
		}
	}
	if _, serr := os.Lstat(keep); serr != nil {
		t.Fatalf("unrelated dir must stay: %v", serr)
	}

	n, err = CleanupDeleting(root, nil)
	if err != nil || n != 0 {
		t.Fatalf("second run must be no-op, got n=%d err=%v", n, err)
	}
}
