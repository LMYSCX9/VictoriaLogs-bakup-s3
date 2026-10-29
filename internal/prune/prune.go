// Package prune 实现 S3 备份单元清理（gc 命令后端，设计文档 §7.4）。
// 清理范围仅限「无 manifest 的未完成前缀」（R-GC-03）；
// 完整单元的人工移除必须走 DeleteUnit 并保持「先 manifest 后数据」顺序（R-GC-02）。
// 按保留期淘汰过期单元是 S3 桶生命周期规则的职责（R-GC-04），本包不提供按日期批量删除。
package prune

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
)

// UnitStore 是 prune 依赖的对象存储窄接口（自带重试，C12）。
type UnitStore interface {
	List(ctx context.Context, prefix string) ([]s3store.ObjectMeta, error)
	Delete(ctx context.Context, key string) error
}

// 编译期断言：*s3store.Store 满足窄接口。
var _ UnitStore = (*s3store.Store)(nil)

// Options 是 Job 的构造参数。
type Options struct {
	StaleAfter time.Duration // 无 manifest 前缀的陈旧阈值（R-GC-03）；≤0 时兜底 24h
	Logger     *slog.Logger
}

// StaleUnit 是一个待清理的未完成前缀（dry-run 清单条目）。
type StaleUnit struct {
	Partition    string    // YYYYMMDD
	Prefix       string    // 备份单元键前缀
	Objects      int       // 前缀内对象数
	Bytes        int64     // 前缀内总字节
	LastModified time.Time // 前缀内对象的最大 LastModified
}

// Plan 是一次只读扫描的结果（R-GC-01 dry-run 输出的数据源）。
type Plan struct {
	Units        []StaleUnit
	TotalObjects int
	TotalBytes   int64
}

// UnitResult 是单单元删除结果摘要（§11.3）。
type UnitResult struct {
	Partition string
	Objects   int
	Bytes     int64
}

// Result 是 gc 执行结果摘要。
type Result struct {
	DryRun  bool
	Plan    Plan         // 本次扫描出的未完成前缀清单
	Deleted []UnitResult // yes 模式下成功清理的单元
	Objects int          // 实际删除对象数（含 manifest）
	Bytes   int64        // 实际释放字节
}

// Job 是 gc 状态机。依赖全部经构造注入（禁止跨层直接 new 客户端）。
type Job struct {
	store UnitStore
	keys  *s3store.KeyBuilder
	log   *slog.Logger
	opts  Options

	// list / delete 是故障注入点（AGENTS §5.4）。
	list   func(ctx context.Context, prefix string) ([]s3store.ObjectMeta, error)
	delete func(ctx context.Context, key string) error
}

// NewJob 构造 gc 作业。
func NewJob(store UnitStore, keys *s3store.KeyBuilder, opts Options) (*Job, error) {
	if store == nil {
		return nil, errs.Usage("object store must not be nil")
	}
	if keys == nil {
		return nil, errs.Usage("key builder must not be nil")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 24 * time.Hour
	}
	return &Job{
		store:  store,
		keys:   keys,
		log:    opts.Logger,
		opts:   opts,
		list:   store.List,
		delete: store.Delete,
	}, nil
}

// Scan 只读扫描 base 下所有日期前缀，返回 stale 清单（R-GC-03）：
// 无 manifest 且最大 LastModified 早于 staleAfter 的前缀。
// 含 manifest 的完整单元一律跳过（R-GC-04）；非日期段键忽略并告警。
func (j *Job) Scan(ctx context.Context, staleAfter time.Duration) (Plan, error) {
	base := j.keys.Base()
	objs, err := j.list(ctx, base)
	if err != nil {
		return Plan{}, errs.Network(err, "list objects under %s", base)
	}

	type unit struct {
		objects     int
		bytes       int64
		last        time.Time
		hasManifest bool
	}
	units := map[string]*unit{}
	var order []string
	for _, o := range objs {
		rel := strings.TrimPrefix(o.Key, base)
		i := strings.Index(rel, "/")
		if i <= 0 || !validPartition(rel[:i]) {
			j.log.Warn("ignore unexpected key under base prefix", slog.String("key", o.Key))
			continue
		}
		part := rel[:i]
		u := units[part]
		if u == nil {
			u = &unit{}
			units[part] = u
			order = append(order, part)
		}
		u.objects++
		u.bytes += o.Size
		if o.LastModified.After(u.last) {
			u.last = o.LastModified
		}
		if rel == part+"/manifest.json" {
			u.hasManifest = true
		}
	}
	sort.Strings(order)

	var plan Plan
	for _, part := range order {
		u := units[part]
		if u.hasManifest {
			continue // 完整单元不在 gc 默认范围（R-GC-04）
		}
		// 最后修改时间未知时保守处理：视为未满 stale-after，不纳入清理。
		if u.last.IsZero() || time.Since(u.last) <= staleAfter {
			continue
		}
		prefix, err := j.keys.UnitPrefix(part)
		if err != nil {
			return plan, err
		}
		plan.Units = append(plan.Units, StaleUnit{
			Partition: part, Prefix: prefix,
			Objects: u.objects, Bytes: u.bytes, LastModified: u.last,
		})
		plan.TotalObjects += u.objects
		plan.TotalBytes += u.bytes
	}
	return plan, nil
}

// Run 执行 gc：Scan → yes 时逐单元 DeleteUnit。
// yes=false 为 dry-run（R-GC-01 默认），不产生任何删除。
// 单元失败不阻塞其余单元（cron 批量场景），错误聚合返回。
func (j *Job) Run(ctx context.Context, yes bool) (Result, error) {
	plan, err := j.Scan(ctx, j.opts.StaleAfter)
	if err != nil {
		return Result{}, err
	}
	res := Result{DryRun: !yes, Plan: plan}
	if !yes || len(plan.Units) == 0 {
		return res, nil
	}
	var joinErr error
	for _, u := range plan.Units {
		r, err := j.DeleteUnit(ctx, u.Partition)
		if err != nil {
			joinErr = errors.Join(joinErr, err)
			continue
		}
		res.Deleted = append(res.Deleted, r)
		res.Objects += r.Objects
		res.Bytes += r.Bytes
	}
	return res, joinErr
}

// DeleteUnit 删除一个备份单元：先 manifest 后数据（R-GC-02，AGENTS §9 禁止颠倒）。
// 对本就无 manifest 的未完成前缀同样适用（无 manifest 则直接删数据）。
// 中断后该前缀处于「无 manifest」状态，不会出现 manifest 指向缺失对象。
func (j *Job) DeleteUnit(ctx context.Context, partition string) (UnitResult, error) {
	res := UnitResult{Partition: partition}
	prefix, err := j.keys.UnitPrefix(partition)
	if err != nil {
		return res, err
	}
	objs, err := j.list(ctx, prefix)
	if err != nil {
		return res, errs.Network(err, "list unit %s", prefix)
	}

	var mkey string
	var manifestSize int64
	var rest []s3store.ObjectMeta
	for _, o := range objs {
		if o.Key == prefix+"manifest.json" {
			mkey, manifestSize = o.Key, o.Size
			continue
		}
		rest = append(rest, o)
	}

	// 第一步：删 manifest（提交点先失效，C3 语义收敛为「无 manifest 未完成前缀」）。
	if mkey != "" {
		j.log.Info("delete unit manifest first", slog.String("key", mkey))
		if err := j.delete(ctx, mkey); err != nil {
			return res, errs.Network(err, "delete manifest %s", mkey)
		}
		res.Objects++
		res.Bytes += manifestSize
	}

	// 第二步：删数据对象。
	for _, o := range rest {
		j.log.Info("delete unit object", slog.String("key", o.Key))
		if err := j.delete(ctx, o.Key); err != nil {
			return res, errs.Network(err, "delete object %s", o.Key)
		}
		res.Objects++
		res.Bytes += o.Size
	}
	return res, nil
}

// validPartition 校验八位 UTC 日期（C8），与 s3store 的键校验规则一致。
func validPartition(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	_, err := time.Parse("20060102", s)
	return err == nil
}
