// LocalStore 是 ObjectStore 的本地文件系统实现。
// 键空间用 "/" 分隔（与 S3 实现一致），落盘时映射到 filepath.Join(baseDir, key)；
// List 返回的 Key 统一为 "/" 分隔格式，编排层无感知。
package s3store

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// LocalOptions 是 LocalStore 的构造参数。
type LocalOptions struct {
	BaseDir string // 备份根目录（绝对路径），必须可写
}

// LocalStore 把 ObjectStore 操作落到本地文件系统上。
type LocalStore struct {
	base string // 已 clean 的绝对路径
}

// 编译期断言：LocalStore 满足 ObjectStore 契约。
var _ ObjectStore = (*LocalStore)(nil)

// NewLocalStore 构造本地存储并确保 BaseDir 存在。
func NewLocalStore(opts LocalOptions) (*LocalStore, error) {
	if opts.BaseDir == "" {
		return nil, errs.Usage("local_backup_path must not be empty")
	}
	if !filepath.IsAbs(opts.BaseDir) {
		return nil, errs.Usage("local_backup_path %q must be absolute", opts.BaseDir)
	}
	b, err := filepath.Abs(filepath.Clean(opts.BaseDir))
	if err != nil {
		return nil, errs.Usagef(err, "resolve local_backup_path %s", opts.BaseDir)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		return nil, errs.Usagef(err, "create local_backup_path %s", b)
	}
	return &LocalStore{base: b}, nil
}

// keyToPath 把 "/" 分隔的对象键映射为本地绝对路径。
// 内部路径遍历一律用 filepath.FromSlash + filepath.Join，再转成键表示；
// 这里只做单向映射，禁止 filepath 反解（反解走 pathToKey）。
func (l *LocalStore) keyToPath(key string) string {
	rel := filepath.FromSlash(key)
	return filepath.Join(l.base, rel)
}

// pathToKey 把本地绝对路径转成 "/" 分隔的对象键。
// 非本存储根的路径返回空串。比较一律大小写敏感：
// List 的路径来自从 base 出发的 WalkDir，大小写与 base 逐字符一致；
// 外部传入的大小写不一致路径视为不属于本存储（防御性拒绝）。
func (l *LocalStore) pathToKey(abs string) string {
	cleaned := filepath.Clean(abs)
	absBase := filepath.Clean(l.base)
	if cleaned != absBase && !strings.HasPrefix(cleaned, absBase+string(os.PathSeparator)) {
		return ""
	}
	rel, err := filepath.Rel(absBase, cleaned)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// ctxReader 包装 io.Reader：每次 Read 前检查 ctx 取消，使本地 IO 可被中断（AGENTS §5.1）。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Head 返回对象元数据；不存在返回 (zero, false, nil)。
func (l *LocalStore) Head(ctx context.Context, key string) (ObjectMeta, bool, error) {
	p := l.keyToPath(key)
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ObjectMeta{}, false, nil
		}
		return ObjectMeta{}, false, errs.Untyped(err, "local head %s", key)
	}
	if fi.IsDir() {
		return ObjectMeta{}, false, nil
	}
	return ObjectMeta{
		Key:          key,
		Size:         fi.Size(),
		LastModified: fi.ModTime().UTC(),
	}, true, nil
}

// Put 写入对象：确保父目录存在，先写临时文件再 rename 原子落位（失败不产生半成品）。
// size 声明用于校验实际写入字节数，不一致视为源数据异常。
func (l *LocalStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if size < 0 {
		return errs.Usage("local put %s: negative size %d", key, size)
	}
	p := l.keyToPath(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return errs.Untyped(err, "local put %s: create dir", key)
	}
	// 用同目录临时文件 + rename 保证原子落位。
	tmp := p + ".tmp-" + fmt.Sprintf("%d", time.Now().UnixNano())
	defer os.Remove(tmp) // 清理任何残留
	f, err := os.Create(tmp)
	if err != nil {
		return errs.Untyped(err, "local put %s: create", key)
	}
	n, err := io.Copy(f, &ctxReader{ctx: ctx, r: r})
	// 无论 io.Copy 是否出错，都先 Close 刷完 OS 缓冲。
	if cerr := f.Close(); cerr != nil {
		return errs.Untyped(cerr, "local put %s: close", key)
	}
	if err != nil {
		return errs.Untyped(err, "local put %s: copy", key)
	}
	if n != size {
		return errs.Untyped(io.ErrUnexpectedEOF, "local put %s: read %d, declared %d", key, n, size)
	}
	if err := os.Rename(tmp, p); err != nil {
		return errs.Untyped(err, "local put %s: rename", key)
	}
	return nil
}

// Get 把对象内容写入 io.WriterAt（顺序从 offset 0 开始；编排层需要并发分段时可自行包装）。
// 这里走简单 File.WriteAt：File 本身实现 WriterAt。
func (l *LocalStore) Get(ctx context.Context, key string, w io.WriterAt) error {
	p := l.keyToPath(key)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return errs.NotFound(err, "local get %s: not found", key)
		}
		return errs.Untyped(err, "local get %s", key)
	}
	defer f.Close()
	if _, err := io.Copy(&offsetWriter{w: w}, &ctxReader{ctx: ctx, r: f}); err != nil {
		return errs.Untyped(err, "local get %s: read", key)
	}
	return nil
}

// List 列举 prefix 下所有对象：WalkDir 扫本存储根，目录按前缀剪枝（不匹配即 SkipDir），
// 文件统一转成 "/" 分隔键再返回；遍历中响应 ctx 取消。
func (l *LocalStore) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	prefix = strings.TrimPrefix(prefix, "/")
	var metas []ObjectMeta
	err := filepath.WalkDir(l.base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return errs.Untyped(err, "local list walk %s", path)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			// 剪枝：目录键既不是 prefix 的前缀段、也不落在 prefix 之下时，整棵子树跳过。
			if key := l.pathToKey(path); key != "" && key != "." {
				dirPrefix := key + "/"
				if !strings.HasPrefix(prefix, dirPrefix) && !strings.HasPrefix(dirPrefix, prefix) {
					return fs.SkipDir
				}
			}
			return nil
		}
		key := l.pathToKey(path)
		if key == "" {
			return nil
		}
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return errs.Untyped(err, "local list stat %s", path)
		}
		metas = append(metas, ObjectMeta{
			Key:          key,
			Size:         fi.Size(),
			LastModified: fi.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return metas, nil
}

// Delete 删除对象；不存在视为成功（与 S3 DeleteObject 行为一致）。
// 删除成功后向上清理已变空的父目录（最多到 base），避免 gc 后残留空目录树。
func (l *LocalStore) Delete(ctx context.Context, key string) error {
	p := l.keyToPath(key)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return errs.Untyped(err, "local delete %s", key)
	}
	// 父目录非空或删除失败即停止（os.Remove 对非空目录报错，无副作用）。
	sep := string(os.PathSeparator)
	for dir := filepath.Dir(p); dir != l.base && strings.HasPrefix(dir, l.base+sep); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			break
		}
	}
	return nil
}

// HeadDir 检查 baseDir 可访问可写（setup 连通性验证用；对应 S3 的 HeadBucket）。
func (l *LocalStore) HeadDir(ctx context.Context) error {
	// 1. 目录存在。
	fi, err := os.Stat(l.base)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(l.base, 0o755); err != nil {
				return errs.Usagef(err, "create local_backup_path %s", l.base)
			}
			fi, err = os.Stat(l.base)
		}
		if err != nil {
			return errs.Untyped(err, "stat local_backup_path %s", l.base)
		}
	}
	if !fi.IsDir() {
		return errs.Usage("local_backup_path %s exists but is not a directory", l.base)
	}
	// 2. 可写：在目录里创建临时文件再删除。
	tf, err := os.CreateTemp(l.base, ".probe-*")
	if err != nil {
		return errs.Usagef(err, "local_backup_path %s is not writable", l.base)
	}
	name := tf.Name()
	_ = tf.Close()
	_ = os.Remove(name)
	return nil
}
