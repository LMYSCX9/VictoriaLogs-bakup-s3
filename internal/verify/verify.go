// Package verify 实现备份单元的在线清单与校验（设计文档 §8.2）：
// list 扫描本节点键空间收集已提交单元；verify 做存在性检查——
// manifest 存在性与版本兼容（C3：无 manifest 的前缀视为残缺单元）→
// 逐对象 HEAD 比对 size（--deep 时再下载全量比对 sha256）。
// 校验只读 S3，不触碰 VL 与本地分区。
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/manifest"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
)

// Options 是 Job 的构造参数；凭据不经过本包（C9）。
type Options struct {
	Concurrency int          // 校验 worker 数；<=0 时默认 8（§8.3）
	Deep        bool         // 逐对象全量下载比对 sha256
	Logger      *slog.Logger //
}

// Result 是单分区备份单元的校验结果摘要（§11.3）。
type Result struct {
	Partition  string
	Files      int      // manifest 声明的文件数
	Bytes      int64    // manifest 声明的总字节
	Deep       bool     // 是否执行了 sha256 全量比对
	Missing    []string // S3 缺失对象的相对路径
	Mismatched []string // size/sha256 不一致的相对路径
}

// OK 报告校验是否全部通过。
func (r *Result) OK() bool {
	return len(r.Missing) == 0 && len(r.Mismatched) == 0
}

// Unit 是 list 子命令输出的已提交备份单元（R-MF-01）。
type Unit struct {
	Partition string
	Files     int   // manifest 声明的文件数
	Bytes     int64 // manifest 声明的总字节
}

// List 扫描本节点键空间（KeyBuilder.Base()）下的全部前缀，只收集含已提交
// manifest 的分区（C3：无 manifest 的前缀视为残缺单元，不得展示或使用），
// 按分区日期升序返回。逐分区并发抓取 manifest 统计 Files/Bytes，并发数来自
// Options.Concurrency；抓取失败按 fetchManifest 归类（Get→网络 3、Parse→校验 5），
// 整体失败返回错误。
func (j *Job) List(ctx context.Context) ([]Unit, error) {
	base := j.keys.Base()
	metas, err := j.store.List(ctx, base)
	if err != nil {
		return nil, errs.Network(err, "list objects under %s", base)
	}

	// 按「<partition>/manifest.json」分组；数据对象与残缺前缀一律忽略。
	const manifestName = "manifest.json"
	sizes := make(map[string]int64)
	for _, m := range metas {
		rel := strings.TrimPrefix(m.Key, base)
		i := strings.Index(rel, "/")
		if i <= 0 || rel[i+1:] != manifestName {
			continue
		}
		sizes[rel[:i]] = m.Size
	}
	if len(sizes) == 0 {
		return []Unit{}, nil
	}

	parts := make([]string, 0, len(sizes))
	for p := range sizes {
		parts = append(parts, p)
	}
	sort.Strings(parts)

	out := make([]Unit, len(parts))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(j.opts.Concurrency)
	for i, part := range parts {
		idx, partition := i, part
		g.Go(func() error {
			mkey, err := j.keys.ManifestKey(partition)
			if err != nil {
				return err
			}
			m, err := j.fetchManifest(gctx, mkey, sizes[partition])
			if err != nil {
				return err
			}
			out[idx] = Unit{Partition: partition, Files: len(m.Entries), Bytes: m.TotalBytes}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	j.log.Info("list done", slog.Int("units", len(out)))
	return out, nil
}

// Job 是单备份单元的在线校验作业。
type Job struct {
	store s3store.ObjectStore
	keys  *s3store.KeyBuilder
	opts  Options
	log   *slog.Logger
}

// NewJob 构造校验作业。
func NewJob(store s3store.ObjectStore, keys *s3store.KeyBuilder, opts Options) (*Job, error) {
	if store == nil {
		return nil, errs.Usage("s3 store must not be nil")
	}
	if keys == nil {
		return nil, errs.Usage("key builder must not be nil")
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Job{store: store, keys: keys, opts: opts, log: opts.Logger}, nil
}

// Run 校验 partition 的备份单元，返回逐项差异；S3 访问失败归类为网络错误（退出码 3）。
func (j *Job) Run(ctx context.Context, partition string) (*Result, error) {
	res := &Result{Partition: partition, Deep: j.opts.Deep}

	// C3：manifest 是唯一提交点；无 manifest 的前缀禁止使用（退出码 4）。
	mkey, err := j.keys.ManifestKey(partition)
	if err != nil {
		return res, err
	}
	meta, ok, err := j.store.Head(ctx, mkey)
	if err != nil {
		return res, errs.Network(err, "head manifest %s", mkey)
	}
	if !ok {
		return res, errs.NotFound(nil, "no committed manifest at %s; incomplete backup unit must not be used", mkey)
	}

	m, err := j.fetchManifest(ctx, mkey, meta.Size)
	if err != nil {
		return res, err
	}
	res.Files = len(m.Entries)
	res.Bytes = m.TotalBytes

	// 逐对象校验：worker 数来自配置（§8.3）；差异只记录不中断，网络错误立即收敛。
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(j.opts.Concurrency)
	for i := range m.Entries {
		e := m.Entries[i]
		g.Go(func() error {
			rel, verdict, err := j.checkOne(gctx, partition, e, res.Deep)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			switch verdict {
			case verdictMissing:
				res.Missing = append(res.Missing, rel)
			case verdictMismatched:
				res.Mismatched = append(res.Mismatched, rel)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return res, err
	}
	j.log.Info("verify done",
		slog.String("partition", partition),
		slog.Int("files", res.Files),
		slog.Int("missing", len(res.Missing)),
		slog.Int("mismatched", len(res.Mismatched)),
		slog.Bool("deep", res.Deep))
	return res, nil
}

// checkOne 校验单个对象：HEAD 比对 size；Deep 时下载全量比对 sha256。
func (j *Job) checkOne(ctx context.Context, partition string, e manifest.Entry, deep bool) (string, verdict, error) {
	key, err := j.keys.FileKey(partition, e.Path)
	if err != nil {
		return e.Path, verdictOK, errs.Validation(err, "build key for %s", e.Path)
	}
	meta, ok, err := j.store.Head(ctx, key)
	if err != nil {
		return e.Path, verdictOK, errs.Network(err, "head object %s", key)
	}
	if !ok {
		return e.Path, verdictMissing, nil
	}
	if meta.Size != e.Size {
		return e.Path, verdictMismatched, nil
	}
	if deep {
		hw := &hashWriter{h: sha256.New()}
		if err := j.store.Get(ctx, key, hw); err != nil {
			return e.Path, verdictOK, errs.Network(err, "get object %s", key)
		}
		if hex.EncodeToString(hw.h.Sum(nil)) != e.SHA256 {
			return e.Path, verdictMismatched, nil
		}
	}
	return e.Path, verdictOK, nil
}

// fetchManifest 下载并解析 manifest（Parse 内含版本与完整性校验，失败归类为校验失败，退出码 5）。
func (j *Job) fetchManifest(ctx context.Context, mkey string, size int64) (*manifest.Manifest, error) {
	buf := make([]byte, 0, max64(size, 512))
	w := &sliceWriter{buf: buf}
	if err := j.store.Get(ctx, mkey, w); err != nil {
		return nil, errs.Network(err, "get manifest %s", mkey)
	}
	m, err := manifest.Parse(w.buf)
	if err != nil {
		return nil, errs.Validation(err, "parse manifest %s", mkey)
	}
	return m, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

type verdict int

const (
	verdictOK verdict = iota
	verdictMissing
	verdictMismatched
)

// sliceWriter 实现 io.WriterAt，把小对象（manifest）累积到内存缓冲。
// S3 分段写入可能乱序，故按任意偏移落位、空洞补零，完整性由 manifest.Parse 兜底。
type sliceWriter struct {
	buf []byte
}

func (w *sliceWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errs.Usage("negative offset %d", off)
	}
	end := int(off) + len(p)
	if end > len(w.buf) {
		grown := make([]byte, end)
		copy(grown, w.buf)
		w.buf = grown
	}
	copy(w.buf[off:], p)
	return len(p), nil
}

// hashWriter 以顺序写方式把对象流喂给 sha256（真实 Store.Get 顺序写入）；
// 乱序写入立即报错，避免静默算出错误摘要。
type hashWriter struct {
	h   hash.Hash
	off int64
}

func (w *hashWriter) WriteAt(p []byte, off int64) (int, error) {
	if off != w.off {
		return 0, errs.Untyped(io.ErrUnexpectedEOF, "non-sequential write at offset %d, want %d", off, w.off)
	}
	if _, err := w.h.Write(p); err != nil {
		return 0, errs.Untyped(err, "hash object stream")
	}
	w.off += int64(len(p))
	return len(p), nil
}
