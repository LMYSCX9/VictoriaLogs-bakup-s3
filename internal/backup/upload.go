package backup

import (
	"context"
	"os"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/vlogs-backup/vlogs-backup/internal/backupmeta"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// entryWithPath 是待上传文件：manifest 条目 + 快照内绝对路径。
type entryWithPath struct {
	backupmeta.Entry
	local string
}

// uploadAll 并发上传快照文件到 S3（§7.1 步骤 4、§8.3 并发模型）：
// worker 数来自配置；已存在且大小一致的对象直接跳过（幂等续传，R-BK-05）；
// 任一失败经 errgroup 取消其余上传，已上传对象保留（C3：无 manifest 则不算备份）。
// 返回跳过对象数；进度按已完成字节与文件数回调（R-BK-08，节流由实现方负责）。
func (j *Job) uploadAll(ctx context.Context, partition string, scanned []entryWithPath) (int, error) {
	totalFiles := len(scanned)
	var totalBytes int64
	for _, e := range scanned {
		totalBytes += e.Size
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(j.opts.Concurrency)

	var mu sync.Mutex
	skipped := 0
	doneFiles := 0
	var doneBytes int64
	bump := func(size int64) {
		mu.Lock()
		doneFiles++
		doneBytes += size
		df, db := doneFiles, doneBytes
		mu.Unlock()
		j.progress(df, totalFiles, db, totalBytes)
	}

	for _, e := range scanned {
		g.Go(func() error {
			key, err := j.keys.FileKey(partition, e.Path)
			if err != nil {
				return err
			}
			if meta, ok, err := j.store.Head(ctx, key); err != nil {
				return err
			} else if ok && meta.Size == e.Size {
				mu.Lock()
				skipped++
				mu.Unlock()
				bump(e.Size)
				return nil
			}
			f, err := os.Open(e.local)
			if err != nil {
				return errs.Untyped(err, "open %s", e.local)
			}
			defer f.Close()
			if err := j.store.Put(ctx, key, f, e.Size); err != nil {
				return err
			}
			bump(e.Size)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return skipped, err
	}
	return skipped, nil
}
