// 下载与校验（C4）：全部文件先落入同文件系统临时目录，逐个校验通过后才允许
// 进入替换阶段；目标分区在下载阶段绝不触碰（R-RS-03）。
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sync/errgroup"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/manifest"
)

// sliceWriter 实现 io.WriterAt，把小对象（manifest）累积到内存缓冲。
// S3 分段写入可能乱序，故按任意偏移落位、空洞补零，完整性由 manifest.Parse 校验兜底。
type sliceWriter struct {
	buf []byte
}

func (w *sliceWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
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

// downloadAll 并发下载 manifest 列出的全部文件到 tmpDir 并逐个校验（R-RS-01/03）。
// worker 数来自配置（§8.3 默认 8）；任一文件失败即收敛整个 errgroup。
func (j *Job) downloadAll(ctx context.Context, partition, tmpDir string, entries []manifest.Entry) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(j.opts.Concurrency)
	for i := range entries {
		e := entries[i]
		g.Go(func() error {
			return j.downloadOne(gctx, partition, tmpDir, e)
		})
	}
	return g.Wait()
}

// downloadOne 下载单个文件到 tmpDir 并校验 size（与 deep 时的 sha256）。
func (j *Job) downloadOne(ctx context.Context, partition, tmpDir string, e manifest.Entry) error {
	key, err := j.keys.FileKey(partition, e.Path)
	if err != nil {
		return errs.Untyped(err, "build key for %s", e.Path)
	}
	dst := filepath.Join(tmpDir, filepath.FromSlash(e.Path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return errs.Untyped(err, "mkdir parent for %s", dst)
	}
	j.log.Info("download file",
		slog.String("key", key), slog.String("dst", dst), slog.Int64("size", e.Size))

	f, err := os.Create(dst)
	if err != nil {
		return errs.Untyped(err, "create %s", dst)
	}
	getErr := j.store.Get(ctx, key, f)
	closeErr := f.Close()
	if getErr != nil {
		return errs.Network(getErr, "get %s", key)
	}
	if closeErr != nil {
		return errs.Untyped(closeErr, "close %s", dst)
	}
	return verifyFile(dst, e, j.opts.Deep)
}

// verifyFile 校验下载产物：size 恒校验；deep 时校验 sha256（R-RS-03，失败分类为 Validation → 退出码 5）。
func verifyFile(path string, e manifest.Entry, deep bool) error {
	fi, err := os.Stat(path)
	if err != nil {
		return errs.Untyped(err, "stat %s", path)
	}
	if fi.Size() != e.Size {
		return errs.Validation(nil, "size mismatch for %s: got %d, want %d", path, fi.Size(), e.Size)
	}
	if deep {
		sum, err := hashFile(path)
		if err != nil {
			return err
		}
		if sum != e.SHA256 {
			return errs.Validation(nil, "sha256 mismatch for %s: got %s, want %s", path, sum, e.SHA256)
		}
	}
	return nil
}

// hashFile 计算文件 sha256 的 hex 编码。
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errs.Untyped(err, "open %s for hash", path)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", errs.Untyped(err, "hash %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
