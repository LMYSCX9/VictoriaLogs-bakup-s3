// Package purge 实现本地分区删除（delete 命令后端，设计文档 §7.5）：
// detach → rename 出 partitions/ → 递归删除 → 复查。
// 硬约束 C13：必须先确认、先 detach、必须 rename 出 partitions/ 后再递归删除；
// 本包不得改动 S3 任何对象（R-DEL-10，仅读 manifest 判断备份护栏）。
// 回滚逻辑见 rollback.go（AGENTS §5.3 分文件要求），确认提示内容见 confirm.go。
package purge

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/lock"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
	"github.com/vlogs-backup/vlogs-backup/internal/vl"
)

// VLClient 是 purge 依赖的 VL 访问窄接口（C1：删除走 detach/attach，不重启进程）。
type VLClient interface {
	Detach(ctx context.Context, partition string) error
	Attach(ctx context.Context, partition string) error
}

// 编译期断言：*vl.Client 满足窄接口。
var _ VLClient = (*vl.Client)(nil)

// BackupChecker 只读备份存在性检查（R-DEL-07 护栏）。delete 禁止任何 S3 写接口（R-DEL-10）。
type BackupChecker interface {
	Head(ctx context.Context, key string) (s3store.ObjectMeta, bool, error)
}

// 编译期断言：*s3store.Store 满足窄接口。
var _ BackupChecker = (*s3store.Store)(nil)

// Options 是 Job 的构造参数；凭据不经过本包（C9）。
type Options struct {
	Cluster  string // 参与 manifest 键布局（C7）
	Node     string
	S3Prefix string
	DataPath string // VL --storageDataPath；partitions/ 与 .deleting-* 均在其下

	ForceNoBackup bool // R-DEL-07：无已提交备份时的二次授权
	ForceToday    bool // R-DEL-08：删除 UTC 今天分区所需的额外授权
	Logger        *slog.Logger
}

// Result 是单分区删除的结果摘要（§11.3）。
type Result struct {
	Partition string
	Path      string // 被删除的分区目录绝对路径
	Files     int
	Bytes     int64
	ElapsedMS int64
}

// Info 是确认提示所需的现场信息（R-DEL-02 要求的六字段来源）。
type Info struct {
	Partition  string // YYYYMMDD
	Path       string // partitions/<partition> 绝对路径
	Exists     bool
	Files      int
	Bytes      int64
	HasBackup  bool   // S3 上该日是否有已提交 manifest（C3 语义）
	BackupKey  string // manifest 对象键（提示中展示）
	UnitPrefix string // 备份单元键前缀（提示中展示无备份位置）
	IsToday    bool   // 是否 UTC 今天（R-DEL-08 风险标注）
}

// Job 是单分区删除状态机。依赖全部经构造注入（禁止跨层直接 new 客户端）。
type Job struct {
	vl     VLClient
	backup BackupChecker
	keys   *s3store.KeyBuilder
	locks  *lock.Manager
	opts   Options
	log    *slog.Logger

	// rename / remove 是故障注入点（AGENTS §5.4：每个失败点必须有对应测试）。
	rename func(oldname, newname string) error
	remove func(path string) error
}

// NewJob 构造删除作业。
func NewJob(vl VLClient, backup BackupChecker, locks *lock.Manager, opts Options) (*Job, error) {
	if vl == nil {
		return nil, errs.Usage("vl client must not be nil")
	}
	if backup == nil {
		return nil, errs.Usage("backup checker must not be nil")
	}
	if locks == nil {
		return nil, errs.Usage("lock manager must not be nil")
	}
	if opts.DataPath == "" {
		return nil, errs.Usage("data path must not be empty")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	abs, err := filepath.Abs(opts.DataPath)
	if err != nil {
		return nil, errs.Usagef(err, "resolve data path %s", opts.DataPath)
	}
	opts.DataPath = abs
	keys, err := s3store.NewKeyBuilder(opts.S3Prefix, opts.Cluster, opts.Node)
	if err != nil {
		return nil, err
	}
	return &Job{
		vl:     vl,
		backup: backup,
		keys:   keys,
		locks:  locks,
		opts:   opts,
		log:    opts.Logger,
		rename: os.Rename,
		remove: os.RemoveAll,
	}, nil
}

// Inspect 只读收集确认提示信息（R-DEL-02），不做护栏判断与任何变更。
func (j *Job) Inspect(ctx context.Context, partition string) (Info, error) {
	info := Info{Partition: partition}
	day, err := parsePartitionDay(partition)
	if err != nil {
		return info, err
	}
	target := filepath.Join(j.opts.DataPath, "partitions", partition)
	info.Path = target
	fi, err := os.Lstat(target)
	switch {
	case err == nil:
		if fi.Mode()&os.ModeSymlink != 0 {
			return info, errs.Usagef(nil, "partition dir %s is a symlink", target)
		}
		if !fi.IsDir() {
			return info, errs.Usagef(nil, "partition path %s is not a directory", target)
		}
		info.Exists = true
	case os.IsNotExist(err):
		// 分区不存在：其余字段仍收集，护栏据此拒绝。
	default:
		return info, errs.Untyped(err, "lstat partition dir %s", target)
	}
	info.IsToday = day.Equal(time.Now().UTC().Truncate(24 * time.Hour))
	if info.Exists {
		files, bytes, err := statDir(target)
		if err != nil {
			return info, err
		}
		info.Files, info.Bytes = files, bytes
	}
	mkey, err := j.keys.ManifestKey(partition)
	if err != nil {
		return info, err
	}
	info.BackupKey = mkey
	unit, err := j.keys.UnitPrefix(partition)
	if err != nil {
		return info, err
	}
	info.UnitPrefix = unit
	_, ok, err := j.backup.Head(ctx, mkey)
	if err != nil {
		return info, errs.Network(err, "head manifest %s", mkey)
	}
	info.HasBackup = ok
	return info, nil
}

// Guard 是确认提示之前的护栏（R-DEL-07/08 与存在性检查），供 cmd 在确认前调用；
// Run 内部会再次执行以防绕过。
func Guard(info Info, forceNoBackup, forceToday bool) error {
	if !info.Exists {
		return errs.NotFound(nil, "local partition %s does not exist at %s", info.Partition, info.Path)
	}
	if info.IsToday && !forceToday {
		// R-DEL-08 验收：删除今天分区默认以退出码 6 结束。
		return errs.Precondition(nil,
			"partition %s is the UTC today partition; deleting it creates a live-log gap; pass --force-today to proceed",
			info.Partition)
	}
	if !info.HasBackup && !forceNoBackup {
		return errs.Precondition(nil,
			"no committed backup for partition %s; deleting local data now would lose it irrecoverably; pass --force-no-backup to proceed",
			info.Partition)
	}
	return nil
}

// Run 执行删除：detach → rename 出 partitions/ → 递归删除 → 复查（设计文档 §7.5）。
// 确认交互由 cmd 层在调用前完成（C13）；本函数内部仍执行护栏以防直接调用绕过。
func (j *Job) Run(ctx context.Context, partition string) (Result, error) {
	start := time.Now()
	res := Result{Partition: partition}

	if _, err := parsePartitionDay(partition); err != nil {
		return res, err
	}
	info, err := j.Inspect(ctx, partition)
	if err != nil {
		return res, err
	}
	if err := Guard(info, j.opts.ForceNoBackup, j.opts.ForceToday); err != nil {
		return res, err
	}

	if err := j.locks.Acquire(partition); err != nil {
		return res, err
	}
	defer func() {
		if rerr := j.locks.Release(partition); rerr != nil {
			j.log.Error("release partition lock failed", slog.String("partition", partition), slog.String("error", rerr.Error()))
		}
	}()

	// R-DEL-05：必须先 detach 再动目录；detach 失败直接失败、不回滚（目录未动，分区仍在线）。
	j.log.Info("detach partition before delete", slog.String("partition", partition))
	if err := j.vl.Detach(ctx, partition); err != nil {
		return res, errs.Network(err, "detach partition %s", partition)
	}

	// R-DEL-06：先 rename 出 partitions/（同文件系统原子），保证 partitions/ 下任何时刻只有完整目录。
	target := info.Path
	deleting := filepath.Join(j.opts.DataPath,
		".deleting-"+partition+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	j.log.Info("rename partition dir out of partitions",
		slog.String("from", target), slog.String("to", deleting))
	if err := j.rename(target, deleting); err != nil {
		// R-DEL-09：detach 成功但 rename 失败 → 分区已摘除、目录未动 → 必须 attach 回来。
		if aerr := j.attachBack(context.WithoutCancel(ctx), partition); aerr != nil {
			j.log.Error("attach back after rename failure failed",
				slog.String("partition", partition), slog.String("error", aerr.Error()))
		}
		return res, errs.Untyped(err, "rename %s to %s", target, deleting)
	}

	// 从 partitions/ 之外递归删除；失败不回滚（已开始删除，无一致性保证），
	// 停在「partitions/ 无该目录 + .deleting 残留 + 人工命令」（§7.5 失败表第 3 行）。
	j.log.Info("remove deleted partition dir", slog.String("dir", deleting))
	if err := j.remove(deleting); err != nil {
		return res, errs.Untyped(err,
			"remove %s failed; leftover dir can be removed manually (e.g. rm -rf %s), partitions/ is clean",
			deleting, deleting)
	}

	// 复查：partitions/<partition> 必须不存在（§7.5 失败表第 4 行）。
	if _, err := os.Lstat(target); err == nil {
		return res, errs.Untyped(nil,
			"partition dir %s still exists after deletion; manual action required before any VL restart", target)
	} else if !os.IsNotExist(err) {
		return res, errs.Untyped(err, "recheck partition dir %s", target)
	}

	res.Path = target
	res.Files = info.Files
	res.Bytes = info.Bytes
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

// parsePartitionDay 校验分区名为 8 位真实 UTC 日期（C8）。
func parsePartitionDay(s string) (time.Time, error) {
	if len(s) != 8 {
		return time.Time{}, errs.Usage("partition %q must be a valid UTC date in YYYYMMDD", s)
	}
	day, err := time.Parse("20060102", s)
	if err != nil {
		return time.Time{}, errs.Usage("partition %q must be a valid UTC date in YYYYMMDD", s)
	}
	return day, nil
}

// statDir 统计目录下的常规文件数与总字节（确认提示「规模」字段）。
func statDir(root string) (int, int64, error) {
	var files int
	var bytes int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			files++
			bytes += fi.Size()
		}
		return nil
	})
	if err != nil {
		return 0, 0, errs.Untyped(err, "stat dir %s", root)
	}
	return files, bytes, nil
}
