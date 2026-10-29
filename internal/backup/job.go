// Package backup 实现单分区备份状态机（设计文档 §7.1、§8.2）：
// 快照创建 → 扫描采集 → 并发上传 → manifest 提交 → 快照清理，
// 含幂等续传（R-BK-05）、if-exists 处理（R-BK-06）、孤立快照清理（R-BK-07）
// 与批量编排（R-BK-02）。备份数据一律来自 VL 快照（C2），manifest 最后写入（C3）。
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/lock"
	"github.com/vlogs-backup/vlogs-backup/internal/manifest"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
	"github.com/vlogs-backup/vlogs-backup/internal/vl"
)

// 已存在备份的处理策略（R-BK-06）。
const (
	IfExistsSkip    = "skip"    // 默认：目标日期已有已提交备份时跳过
	IfExistsReplace = "replace" // 重新备份：先传新对象与新 manifest，再清理旧引用（§7.1 约束 3）
)

// VLClient 是 backup 依赖的 VL 访问窄接口（设计文档 §8.1），*vl.Client 满足。
// 访问层自带重试，本包不重试（C12）。
type VLClient interface {
	Probe(ctx context.Context) error
	ForceFlush(ctx context.Context) error
	SnapshotCreate(ctx context.Context, partitionPrefix string) ([]string, error)
	SnapshotDelete(ctx context.Context, path string) error
	SnapshotList(ctx context.Context) ([]string, error)
	AppVersion(ctx context.Context) string
}

// 编译期断言：*vl.Client 满足窄接口。
var _ VLClient = (*vl.Client)(nil)

// Options 是 Job 的构造参数；凭据不经过本包（C9）。
type Options struct {
	Cluster      string // 备份单元身份，参与键布局（C7）
	Node         string
	S3Prefix     string // 键前缀，可为空
	Concurrency  int    // 上传 worker 数；<=0 时默认 8（§8.3）
	IfExists     string // IfExistsSkip（默认）| IfExistsReplace
	ToolVersion  string // 写入 manifest.tool_version
	KeepSnapshot bool   // --keep-snapshot：排障保留快照（R-BK-07，摘要必须提示占用）
	// OrphanSnapshotAge 是孤立快照年龄阈值（R-BK-07）；<=0 时默认 2h。
	OrphanSnapshotAge time.Duration
	// OnProgress 在每完成一个文件（上传或跳过）后被并发调用（R-BK-08），
	// 节流与线程安全由实现方负责；nil 表示不需要进度。
	OnProgress func(doneFiles, totalFiles int, doneBytes, totalBytes int64)
	Logger     *slog.Logger
}

// Result 是单分区备份的结果摘要（§11.3：分区/文件数/总字节/跳过对象数/耗时/快照清理）。
type Result struct {
	Partition       string
	Files           int   // manifest 收录的文件数
	Bytes           int64 // manifest 总字节
	Skipped         int   // 幂等续传跳过的对象数（R-BK-05）
	SkippedExisting bool  // if-exists=skip 命中已有备份（R-BK-06）
	Replaced        bool  // if-exists=replace 完成重建
	Today           bool  // UTC 当天分区，已强制落盘（R-BK-04）
	SnapshotCleaned bool  // 快照是否已清理；KeepSnapshot 时为 false（R-BK-07）
	ElapsedMS       int64
	VLVersion       string
}

// Job 是单分区备份状态机。依赖全部经构造注入（禁止跨层直接 new 客户端），
// 一个 Job 可按日期反复 Run（批量场景复用，锁与键均按分区参数化）。
type Job struct {
	vl    VLClient
	store s3store.ObjectStore
	keys  *s3store.KeyBuilder
	locks *lock.Manager
	opts  Options
	log   *slog.Logger
}

// NewJob 构造备份作业。
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
	if opts.ToolVersion == "" {
		return nil, errs.Usage("tool version must not be empty")
	}
	switch opts.IfExists {
	case "":
		opts.IfExists = IfExistsSkip
	case IfExistsSkip, IfExistsReplace:
	default:
		return nil, errs.Usage("if-exists must be %s or %s, got %q", IfExistsSkip, IfExistsReplace, opts.IfExists)
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.OrphanSnapshotAge <= 0 {
		opts.OrphanSnapshotAge = 2 * time.Hour
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	keys, err := s3store.NewKeyBuilder(opts.S3Prefix, opts.Cluster, opts.Node)
	if err != nil {
		return nil, err
	}
	return &Job{
		vl:    vl,
		store: store,
		keys:  keys,
		locks: locks,
		opts:  opts,
		log:   opts.Logger,
	}, nil
}

// Run 对单个分区执行完整备份（§7.1 时序）。失败时同样返回已填充的部分结果供渲染；
// 快照在作业结束（无论成败）时清理（C6），manifest 仅在全部数据就绪后写入（C3）。
func (j *Job) Run(ctx context.Context, partition string) (*Result, error) {
	start := time.Now()
	if !validPartition(partition) {
		return nil, errs.Usage("partition %q must be a valid UTC date in YYYYMMDD", partition)
	}
	// 端点能力探测（R-OPS-04）：快照/分区管理端点缺失时 VL 版本过低，作业不得部分执行。
	if err := j.vl.Probe(ctx); err != nil {
		return nil, err
	}
	// 孤立快照清理（R-BK-07）：在获取自身作业锁之前执行，尽力而为不阻塞主流程。
	j.sweepOrphanSnapshots(ctx)
	// 作业锁（§8.2）：同节点同日期严格互斥；被持有时退出码 6。
	if err := j.locks.Acquire(partition); err != nil {
		return nil, err
	}
	defer func() {
		if err := j.locks.Release(partition); err != nil {
			j.warn(ctx, "release partition lock failed", "partition", partition, "err", err)
		}
	}()

	res := &Result{Partition: partition}
	defer func() { res.ElapsedMS = time.Since(start).Milliseconds() }()

	// 已提交备份处理（R-BK-06）：skip 策略下无需创建快照，直接返回。
	mkey, err := j.keys.ManifestKey(partition)
	if err != nil {
		return nil, err
	}
	_, existing, err := j.store.Head(ctx, mkey)
	if err != nil {
		return res, err
	}
	if existing && j.opts.IfExists == IfExistsSkip {
		res.SkippedExisting = true
		res.SnapshotCleaned = true // 未创建快照，无需清理
		return res, nil
	}
	replaced := existing && j.opts.IfExists == IfExistsReplace

	// 当天分区先强制落盘（§7.1 顺序约束 1，R-BK-04）：避免内存缓冲未写入导致快照缺尾部数据。
	vlVer := j.vl.AppVersion(ctx)
	if vlVer == "" {
		vlVer = "unknown"
	}
	if partition == utcToday() {
		if err := j.vl.ForceFlush(ctx); err != nil {
			return res, err
		}
		res.Today = true
	}

	// 创建快照（C2）：这是备份数据的唯一合法来源。
	snapshotAt := time.Now().UTC()
	paths, err := j.vl.SnapshotCreate(ctx, partition)
	if err != nil {
		return res, err
	}
	if len(paths) == 0 {
		return res, errs.NotFound(nil,
			"partition %s not found in vl: check partitions via POST /internal/partition/list and ensure data_path matches the vl --storageDataPath on the same host", partition)
	}
	// 快照清理（C6）：作业结束（无论成败）必达；keep-snapshot 排障时保留并告警。
	defer j.cleanupSnapshots(context.WithoutCancel(ctx), paths, res)

	// 过滤本分区快照目录并扫描采集（相对路径以快照根为基准，恢复时还原 partitions/<date>/ 结构）。
	dirs := filterPartitionSnapshots(paths, partition)
	if len(dirs) == 0 {
		j.warn(ctx, "vl snapshot paths do not match partition layout",
			"partition", partition, "snapshots", strings.Join(paths, ","))
		return res, errs.NotFound(nil,
			"vl returned no snapshot for partition %s: ensure data_path matches the vl --storageDataPath on the same host", partition)
	}
	scanned, err := scanSnapshotDirs(dirs)
	if err != nil {
		return res, err
	}
	if len(scanned) == 0 {
		return res, errs.Untyped(nil, "snapshot of partition %s contains no files", partition)
	}

	// 并发上传（§8.3）：任一失败即取消其余上传，已上传对象保留（幂等续传前提，R-BK-05）。
	skipped, err := j.uploadAll(ctx, partition, scanned)
	if err != nil {
		return res, err
	}
	res.Skipped = skipped

	// manifest 最后写入（C3 唯一提交点）：仅在全部数据对象就绪后上传。
	entries := make([]manifest.Entry, len(scanned))
	var total int64
	for i, e := range scanned {
		entries[i] = e.Entry
		total += e.Size
	}
	m := manifest.Manifest{
		FormatVersion: manifest.FormatVersion,
		Cluster:       j.opts.Cluster,
		Node:          j.opts.Node,
		Partition:     partition,
		CreatedAt:     time.Now().UTC(),
		SnapshotAt:    snapshotAt,
		VLVersion:     vlVer,
		ToolVersion:   j.opts.ToolVersion,
		FileCount:     len(entries),
		TotalBytes:    total,
		Entries:       entries,
	}
	data, err := json.Marshal(&m)
	if err != nil {
		return res, errs.Untyped(err, "marshal manifest for partition %s", partition)
	}
	if err := j.store.Put(ctx, mkey, bytes.NewReader(data), int64(len(data))); err != nil {
		return res, err
	}

	// replace（R-BK-06）：新 manifest 已提交，清理旧 manifest 时代不再被引用的对象
	//（§7.1 约束 3：先传后删）。清理失败只告警——孤儿可由后续 replace 或 gc 兜底。
	if replaced {
		j.pruneStale(ctx, partition, &m, mkey)
		res.Replaced = true
	}

	res.Files = m.FileCount
	res.Bytes = m.TotalBytes
	res.VLVersion = vlVer
	return res, nil
}

// RunMany 按日期逐天独立备份（R-BK-02）：单天失败不影响其余天，
// ctx 取消时中止后续天。返回已执行各天的结果；错误为各天中最严重者（退出码最大）。
func (j *Job) RunMany(ctx context.Context, partitions []string) ([]Result, error) {
	var out []Result
	var worst error
	for _, p := range partitions {
		if err := ctx.Err(); err != nil {
			if worst == nil {
				worst = errs.Canceled("backup batch canceled: %v", err)
			}
			break
		}
		res, err := j.Run(ctx, p)
		if res != nil {
			out = append(out, *res)
		}
		if err != nil && (worst == nil || errs.ExitCode(err) > errs.ExitCode(worst)) {
			worst = err
		}
	}
	return out, worst
}

// ExpandDates 展开批量参数为升序 UTC 日期列表（C8）。
// latest > 0 表示最近 N 天（含 UTC 今天），不得与 from/to 同用；
// from/to 必须成对且 from <= to；三者皆空返回 nil（单日 --date 路径）。
func ExpandDates(from, to string, latest int, now time.Time) ([]string, error) {
	switch {
	case latest > 0:
		if from != "" || to != "" {
			return nil, errs.Usage("--latest cannot be combined with --from/--to")
		}
		end := now.UTC()
		out := make([]string, 0, latest)
		for i := latest - 1; i >= 0; i-- {
			out = append(out, end.AddDate(0, 0, -i).Format("20060102"))
		}
		return out, nil
	case from != "" && to != "":
		f, ferr := time.ParseInLocation("20060102", from, time.UTC)
		t, terr := time.ParseInLocation("20060102", to, time.UTC)
		if ferr != nil {
			return nil, errs.Usage("--from %q must be YYYYMMDD", from)
		}
		if terr != nil {
			return nil, errs.Usage("--to %q must be YYYYMMDD", to)
		}
		if f.After(t) {
			return nil, errs.Usage("--from %s is after --to %s", from, to)
		}
		var out []string
		for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
			out = append(out, d.Format("20060102"))
		}
		return out, nil
	case from != "" || to != "":
		return nil, errs.Usage("--from and --to must be used together")
	default:
		return nil, nil
	}
}

// cleanupSnapshots 删除本次作业创建的全部快照（C6）；keep-snapshot 时保留并告警
// （R-BK-07：排障占用不随保留策略释放，摘要必须提示）。
// 使用 WithoutCancel 的 ctx：作业失败/取消后清理仍必须执行。
func (j *Job) cleanupSnapshots(ctx context.Context, paths []string, res *Result) {
	if j.opts.KeepSnapshot {
		j.warn(ctx, "snapshots kept for troubleshooting, disk usage will not be freed by retention",
			"count", len(paths))
		return // res.SnapshotCleaned 保持 false
	}
	for _, p := range paths {
		if err := j.vl.SnapshotDelete(ctx, p); err != nil {
			j.warn(ctx, "snapshot cleanup failed", "path", p, "err", err)
		}
	}
	res.SnapshotCleaned = true
}

// pruneStale 在 replace 提交新 manifest 后清理不再被引用的旧对象。
// 只告警不失败：新备份已完整可用，孤儿对象由后续 replace 或 gc 兜底。
func (j *Job) pruneStale(ctx context.Context, partition string, m *manifest.Manifest, mkey string) {
	unit, err := j.keys.UnitPrefix(partition)
	if err != nil {
		j.warn(ctx, "replace cleanup skipped", "partition", partition, "err", err)
		return
	}
	metas, err := j.store.List(ctx, unit)
	if err != nil {
		j.warn(ctx, "replace cleanup skipped: list failed", "partition", partition, "err", err)
		return
	}
	keep := make(map[string]struct{}, len(m.Entries)+1)
	keep[mkey] = struct{}{}
	for _, e := range m.Entries {
		k, err := j.keys.FileKey(partition, e.Path)
		if err != nil {
			j.warn(ctx, "replace cleanup skipped: bad entry path", "path", e.Path, "err", err)
			return
		}
		keep[k] = struct{}{}
	}
	for _, meta := range metas {
		if _, ok := keep[meta.Key]; ok {
			continue
		}
		if err := j.store.Delete(ctx, meta.Key); err != nil {
			j.warn(ctx, "replace cleanup delete failed", "key", meta.Key, "err", err)
		}
	}
}

// progress 上报进度；并发调用，线程安全由 OnProgress 实现方保证（R-BK-08）。
func (j *Job) progress(done, total int, doneBytes, totalBytes int64) {
	if j.opts.OnProgress != nil {
		j.opts.OnProgress(done, total, doneBytes, totalBytes)
	}
}

func (j *Job) warn(ctx context.Context, msg string, args ...any) {
	j.log.WarnContext(ctx, msg, args...)
}

// utcToday 返回 UTC 今天（C8：当天判断一律按 UTC）。
func utcToday() string {
	return time.Now().UTC().Format("20060102")
}

// validPartition 校验 8 位数字且为真实日历日期（C8，与 keybuilder/manifest 规则一致）。
func validPartition(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	_, err := time.ParseInLocation("20060102", s, time.UTC)
	return err == nil
}
