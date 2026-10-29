// Package restore 实现单分区在线恢复状态机（设计文档 §7.2、§8.2）：
// manifest 校验 → 保留窗口/磁盘预检 → 并发下载到临时目录 → detach →
// 原子 rename 替换 → attach → 冒烟查询。
// 恢复不重启 VL（C1）；先下载校验后 rename（C4）；无 manifest 的前缀禁止恢复（C3）；
// 任一失败分支必须把分区退回 present 态或停在「分区缺失 + 人工指引」（C5），
// 回滚矩阵见 §7.3，实现位于 rollback.go（AGENTS §5.3 分文件要求）。
package restore

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/backupmeta"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/lock"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
	"github.com/vlogs-backup/vlogs-backup/internal/vl"
)

// 目标分区已存在时的旧目录处置策略（R-RS-06）。
const (
	OnExistingKeep   = "keep"   // 默认：旧目录改名为 <partition>.replaced-<ts> 保留在 dataPath 下
	OnExistingDelete = "delete" // 删除旧目录（下载与校验全部通过之后才执行）
)

// 冒烟查询结果（R-RS-07）。
const (
	SmokeOK     = "ok"     // 查到该日数据
	SmokeEmpty  = "empty"  // 未查到数据：仅告警，不改退出码
	SmokeFailed = "failed" // 查询自身失败：仅告警，不改退出码
)

// VLClient 是 restore 依赖的 VL 访问窄接口（设计文档 §8.1），*vl.Client 满足。
// 访问层自带重试，本包除 attach 按矩阵行④补试一次外不再重试（C12）。
type VLClient interface {
	Detach(ctx context.Context, partition string) error
	Attach(ctx context.Context, partition string) error
	SmokeQuery(ctx context.Context, partition string) (bool, error)
}

// 编译期断言：*vl.Client 满足窄接口。
var _ VLClient = (*vl.Client)(nil)

// Options 是 Job 的构造参数；凭据不经过本包（C9）。
type Options struct {
	Cluster  string // 备份单元身份，参与键布局与 manifest 比对（C7/R-MF-04）
	Node     string
	S3Prefix string // 键前缀，可为空
	DataPath string // VL --storageDataPath；partitions/ 与临时目录均在其下

	RetentionDays int // vl_retention_days；<=0 时默认 181（R-RS-10）
	Concurrency   int // 下载 worker 数；<=0 时默认 8（§8.3）
	Deep          bool
	OnExisting    string // OnExistingKeep（默认）| OnExistingDelete
	ForceToday    bool   // R-RS-08：恢复 UTC 今天分区所需的额外授权

	// AllowNodeMismatch 放行 cluster/node 身份不一致（R-MF-04）；
	// 分区（partition）不一致不受本开关影响，永远拒绝（C8 三方一致）。
	AllowNodeMismatch bool

	// DiskCheckFactor 磁盘预检因子：要求可用空间 ≥ 分区字节 × 该因子（R-RS-02）；<=0 时默认 1.1。
	DiskCheckFactor float64
	Logger          *slog.Logger
}

// Result 是单分区恢复的结果摘要（§11.3）。
type Result struct {
	Partition    string
	Files        int
	Bytes        int64
	Replaced     bool   // 目标分区已存在且完成替换
	ReplacedPath string // 旧目录绝对路径（keep 策略；delete 策略为空，R-RS-06）
	Attached     bool
	Smoke        string // SmokeOK | SmokeEmpty | SmokeFailed
	ElapsedMS    int64
	CrossNode    bool // R-MF-04：cluster/node 不一致经 --allow-node-mismatch 放行
}

// Job 是单分区恢复状态机。依赖全部经构造注入（禁止跨层直接 new 客户端）。
type Job struct {
	vl    VLClient
	store s3store.ObjectStore
	keys  *s3store.KeyBuilder
	locks *lock.Manager
	opts  Options
	log   *slog.Logger

	// diskFree / rename 是故障注入点（AGENTS §5.4：每个失败点必须有对应测试），
	// 仅本包测试可替换；生产路径为平台实现与 os.Rename。
	diskFree func(path string) (uint64, error)
	rename   func(oldname, newname string) error
}

// NewJob 构造恢复作业。
func NewJob(vl VLClient, store s3store.ObjectStore, locks *lock.Manager, opts Options) (*Job, error) {
	if vl == nil {
		return nil, errs.Usage("vl client must not be nil")
	}
	if store == nil {
		return nil, errs.Usage("s3 store must not be nil")
	}
	if locks == nil {
		return nil, errs.Usage("lock manager must not be nil")
	}
	if opts.DataPath == "" {
		return nil, errs.Usage("data path must not be empty")
	}
	switch opts.OnExisting {
	case "":
		opts.OnExisting = OnExistingKeep
	case OnExistingKeep, OnExistingDelete:
	default:
		return nil, errs.Usage("on-existing must be %s or %s, got %q", OnExistingKeep, OnExistingDelete, opts.OnExisting)
	}
	if opts.RetentionDays <= 0 {
		opts.RetentionDays = 181
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.DiskCheckFactor <= 0 {
		opts.DiskCheckFactor = 1.1
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
		vl:       vl,
		store:    store,
		keys:     keys,
		locks:    locks,
		opts:     opts,
		log:      opts.Logger,
		diskFree: diskFree,
		rename:   os.Rename,
	}, nil
}

// Run 执行单分区恢复。时序与失败回滚严格对照设计文档 §7.2 / §7.3。
func (j *Job) Run(ctx context.Context, partition string) (Result, error) {
	start := time.Now()
	res := Result{Partition: partition}

	day, err := parsePartitionDay(partition)
	if err != nil {
		return res, err
	}

	// R-RS-10 保留窗口前置校验：先于任何 S3 请求；边界日期（恰好等于窗口）允许。
	cutoff := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -j.opts.RetentionDays)
	if day.Before(cutoff) {
		return res, errs.Precondition(nil,
			"partition %s is older than retention window (%d days, cutoff %s); refusing to restore to avoid VL native GC reclaim",
			partition, j.opts.RetentionDays, cutoff.Format("20060102"))
	}

	// R-RS-08 当天分区保护：非交互缺 --force-today 按参数缺失处理（退出码 2）。
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if day.Equal(today) && !j.opts.ForceToday {
		return res, errs.Usage(
			"partition %s is the UTC today partition; restoring it requires extra authorization (--force-today)", partition)
	}

	// R-RS-09 路径安全：拒绝符号链接与非法目标。
	partitionsDir := filepath.Join(j.opts.DataPath, "partitions")
	if err := checkDirReal(partitionsDir); err != nil {
		return res, err
	}
	target := filepath.Join(partitionsDir, partition)
	targetExists := false
	if fi, err := os.Lstat(target); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return res, errs.Usagef(nil, "restore target %s is a symlink", target)
		}
		if !fi.IsDir() {
			return res, errs.Usagef(nil, "restore target %s exists but is not a directory", target)
		}
		targetExists = true
	} else if !os.IsNotExist(err) {
		return res, errs.Untyped(err, "lstat restore target %s", target)
	}

	// C3：manifest 是唯一提交点，无 manifest 的前缀禁止恢复。
	mkey, err := j.keys.ManifestKey(partition)
	if err != nil {
		return res, err
	}
	meta, ok, err := j.store.Head(ctx, mkey)
	if err != nil {
		return res, errs.Network(err, "head manifest %s", mkey)
	}
	if !ok {
		return res, errs.NotFound(nil, "no committed manifest at %s; incomplete backup unit must not be restored", mkey)
	}

	mw := &sliceWriter{buf: make([]byte, 0, max64(meta.Size, 512))}
	if err := j.store.Get(ctx, mkey, mw); err != nil {
		return res, errs.Network(err, "get manifest %s", mkey)
	}
	m, err := backupmeta.Parse(mw.buf)
	if err != nil {
		return res, errs.Validation(err, "parse manifest %s", mkey)
	}

	// R-MF-04 身份比对（C8：--date 与 manifest partition 三方一致，永远拒绝）。
	if m.Partition != partition {
		return res, errs.Validation(nil,
			"manifest partition mismatch: manifest says %s, want %s", m.Partition, partition)
	}
	// R-MF-04：cluster/node 不一致默认拒绝；--allow-node-mismatch 显式放行并在摘要标注跨节点恢复。
	if m.Cluster != j.opts.Cluster || m.Node != j.opts.Node {
		if !j.opts.AllowNodeMismatch {
			return res, errs.Validation(nil,
				"manifest identity mismatch: manifest cluster=%s node=%s, want cluster=%s node=%s; pass --allow-node-mismatch to override",
				m.Cluster, m.Node, j.opts.Cluster, j.opts.Node)
		}
		res.CrossNode = true
		j.log.Warn("cross-node restore authorized",
			slog.String("partition", partition),
			slog.String("manifest_cluster", m.Cluster), slog.String("manifest_node", m.Node),
			slog.String("want_cluster", j.opts.Cluster), slog.String("want_node", j.opts.Node))
	}

	// R-RS-02 磁盘预检：不足时退出码 6，且先于任何文件下载。
	required := uint64(math.Ceil(float64(m.TotalBytes) * j.opts.DiskCheckFactor))
	free, err := j.diskFree(partitionsDir)
	if err != nil {
		return res, errs.Untyped(err, "check disk free for %s", partitionsDir)
	}
	if free < required {
		return res, errs.Precondition(nil,
			"disk free %d bytes is below required %d bytes (partition %s totals %d bytes x factor %.2f)",
			free, required, partition, m.TotalBytes, j.opts.DiskCheckFactor)
	}

	if err := j.locks.Acquire(partition); err != nil {
		return res, err
	}
	defer func() {
		if rerr := j.locks.Release(partition); rerr != nil {
			j.log.Error("release partition lock failed", slog.String("partition", partition), slog.String("error", rerr.Error()))
		}
	}()

	tmpDir, err := os.MkdirTemp(j.opts.DataPath, ".restore-"+partition+"-")
	if err != nil {
		return res, errs.Untyped(err, "create temp dir under %s", j.opts.DataPath)
	}
	renamed := false
	// 矩阵行①：下载/校验阶段失败 → 删除临时目录，目标分区未动（C4）。
	defer func() {
		if renamed {
			return
		}
		if rerr := os.RemoveAll(tmpDir); rerr != nil {
			j.log.Error("remove temp dir failed", slog.String("dir", tmpDir), slog.String("error", rerr.Error()))
			return
		}
		j.log.Info("temp dir removed after failure", slog.String("dir", tmpDir))
	}()

	// R-RS-03 校验先于替换：全部文件下载并校验通过之前不触碰目标分区。
	if err := j.downloadAll(ctx, partition, tmpDir, m.Entries); err != nil {
		return res, err
	}
	j.log.Info("download and verify completed",
		slog.String("partition", partition),
		slog.Int("files", m.FileCount), slog.Int64("bytes", m.TotalBytes))

	// rename 前放开临时目录权限，避免 VL 进程（可能不同用户）无法遍历。
	if err := os.Chmod(tmpDir, 0o755); err != nil {
		return res, errs.Untyped(err, "chmod temp dir %s", tmpDir)
	}

	st := &restoreState{
		partition: partition,
		dataPath:  j.opts.DataPath,
		target:    target,
		tmpDir:    tmpDir,
	}
	rbctx := context.WithoutCancel(ctx)

	// detach（仅当目标分区存在；C1：恢复只走 detach → 替换 → attach）。
	if targetExists {
		j.log.Info("detach partition", slog.String("partition", partition))
		if err := j.vl.Detach(ctx, partition); err != nil {
			// detach 失败：本地目录未动；分区可能已实际摘除，尽力 attach 回 present 后报告主失败。
			if aerr := j.vl.Attach(rbctx, partition); aerr != nil {
				j.log.Error("best-effort attach after detach failure failed too",
					slog.String("partition", partition), slog.String("error", aerr.Error()))
			}
			return res, errs.Network(err, "detach partition %s", partition)
		}
		st.detached = true
	}

	// 旧目录处置（R-RS-06）：发生在全部校验通过之后，绝不「先删旧目录再下载新数据」。
	if targetExists {
		switch j.opts.OnExisting {
		case OnExistingDelete:
			j.log.Info("remove existing partition dir", slog.String("path", target), slog.String("policy", j.opts.OnExisting))
			if err := os.RemoveAll(target); err != nil {
				return j.finishRollback(res, st, rbctx, errs.Untyped(err, "remove existing dir %s", target))
			}
			st.oldGone = true
			res.Replaced = true
		default: // OnExistingKeep
			replacedPath := filepath.Join(j.opts.DataPath, partition+".replaced-"+strconv.FormatInt(time.Now().UnixNano(), 10))
			j.log.Info("rename existing partition dir", slog.String("from", target), slog.String("to", replacedPath))
			if err := j.rename(target, replacedPath); err != nil {
				// 矩阵行②：分区已摘除、旧目录仍在 → 回滚 attach 原目录。
				return j.finishRollback(res, st, rbctx, errs.Untyped(err, "rename %s to %s", target, replacedPath))
			}
			st.oldMoved, st.oldPath = true, replacedPath
			res.Replaced, res.ReplacedPath = true, replacedPath
		}
	}

	// 原子替换（C4/R-RS-04）：同文件系统 rename，绝不向目标目录逐文件写入。
	j.log.Info("rename downloaded dir into place", slog.String("from", tmpDir), slog.String("to", target))
	if err := j.rename(tmpDir, target); err != nil {
		// 矩阵行③：旧目录已移走/已删 → 回滚恢复旧目录或落位新目录。
		return j.finishRollback(res, st, rbctx, errs.Untyped(err, "rename %s to %s", tmpDir, target))
	}
	renamed = true
	st.newPath, st.tmpDir = true, ""

	// attach（矩阵行④：失败立即补试一次，仍失败停在现场 + 人工指引，不做目录级回滚）。
	j.log.Info("attach partition", slog.String("partition", partition))
	if err := j.vl.Attach(ctx, partition); err != nil {
		j.log.Error("attach failed, retrying once", slog.String("partition", partition), slog.String("error", err.Error()))
		if err2 := j.vl.Attach(ctx, partition); err2 != nil {
			return res, j.manualAttachError(st, err2)
		}
	}
	res.Attached = true

	// 冒烟查询（R-RS-07）：空数据或查询失败仅告警，不改退出码。
	switch found, serr := j.vl.SmokeQuery(ctx, partition); {
	case serr != nil:
		res.Smoke = SmokeFailed
		j.log.Warn("smoke query failed", slog.String("partition", partition), slog.String("error", serr.Error()))
	case !found:
		res.Smoke = SmokeEmpty
		j.log.Warn("smoke query returned no data; partition attached but appears empty", slog.String("partition", partition))
	default:
		res.Smoke = SmokeOK
	}

	res.Files = m.FileCount
	res.Bytes = m.TotalBytes
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

// finishRollback 执行回滚矩阵并包装主失败：
// 回滚成功 → 返回主失败（其分类即对外退出码）；回滚自身失败 → 矩阵行⑤：终止、
// 输出现场状态与人工命令，退出码 1。
func (j *Job) finishRollback(res Result, st *restoreState, ctx context.Context, cause error) (Result, error) {
	if rberr := j.rollbackToPresent(ctx, st); rberr != nil {
		return res, errs.Untyped(errors.Join(cause, rberr),
			"rollback incomplete, manual action required; state: %s; attach api: POST <vl-url>/internal/partition/attach?name=%s; old data dir: %s",
			st.summary(), st.partition, st.oldDirHint())
	}
	return res, cause
}

// manualAttachError 构造矩阵行④的终态错误：目录一致但未挂载，附人工命令（含绝对路径）。
func (j *Job) manualAttachError(st *restoreState, cause error) error {
	return errs.Untyped(cause,
		"attach failed for partition %s after replace; directories are consistent but partition is not mounted; state: %s; manual: POST <vl-url>/internal/partition/attach?name=%s; new data dir: %s; old data dir: %s",
		st.partition, st.summary(), st.partition, st.target, st.oldDirHint())
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

// checkDirReal 校验 partitions 目录存在、非符号链接且为目录（R-RS-09）。
func checkDirReal(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return errs.Usagef(err, "lstat partitions dir %s (check data-path)", dir)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errs.Usagef(nil, "partitions dir %s is a symlink", dir)
	}
	if !fi.IsDir() {
		return errs.Usagef(nil, "partitions dir %s is not a directory", dir)
	}
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
