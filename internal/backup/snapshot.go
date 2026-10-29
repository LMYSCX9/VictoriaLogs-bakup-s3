package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/backupmeta"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// sweepOrphanSnapshots 清理孤立快照（R-BK-07、设计文档 §7.6）：
// 列举全部快照 → 过滤「分区快照形态 + 存在超过阈值 + 对应分区无作业锁」→ 删除并告警。
// 尽力而为：列举失败只告警，不阻塞备份主流程（孤立快照不影响正确性，仅占磁盘）。
// 必须在获取自身作业锁之前调用：对候选分区 try-Acquire 成功即证明无其他作业持有；
// 失败（被持锁）说明有作业在跑，其快照不是孤立的。锁在判定后立即释放，
// 本进程后续 Run 内对同一分区的 Acquire 幂等且互不干扰。
func (j *Job) sweepOrphanSnapshots(ctx context.Context) {
	paths, err := j.vl.SnapshotList(ctx)
	if err != nil {
		j.warn(ctx, "orphan snapshot sweep skipped: list failed", "err", err)
		return
	}
	for _, p := range paths {
		part := snapshotPartition(p)
		if part == "" {
			continue // 非分区快照形态（不属本工具管理的命名），不动
		}
		info, err := os.Stat(p)
		if err != nil {
			j.warn(ctx, "orphan snapshot stat failed, skip", "path", p, "err", err)
			continue
		}
		age := time.Since(info.ModTime())
		if age < j.opts.OrphanSnapshotAge {
			continue // 年轻快照可能是失败作业的残留，留出保守护栏
		}
		if err := j.locks.Acquire(part); err != nil {
			j.log.InfoContext(ctx, "orphan snapshot candidate is owned by a running job, skip",
				"path", p, "partition", part)
			continue
		}
		if err := j.vl.SnapshotDelete(ctx, p); err != nil {
			j.warn(ctx, "orphan snapshot delete failed", "path", p, "err", err)
		} else {
			j.warn(ctx, "orphan snapshot removed",
				"path", p, "partition", part, "age", age.Round(time.Second).String())
		}
		if err := j.locks.Release(part); err != nil {
			j.warn(ctx, "release sweep lock failed", "partition", part, "err", err)
		}
	}
}

// snapshotPartition 从 VL 快照路径解析分区日期（C8 三方一致的第二道防线）。
// 真实路径形态为 <storageDataPath>/partitions/<YYYYMMDD>/snapshots/<snapshotName>：
// 基名是 VL 生成的快照名而非日期，分区日期位于 snapshots 段的父段，
// 仅识别该形态且父段为合法 UTC 日期时返回，其余一律空串。
// 兼容 POSIX 与 Windows 分隔符及尾部分隔符（VL 返回的路径形态随平台）。
func snapshotPartition(p string) string {
	normalized := strings.TrimRight(strings.ReplaceAll(p, `\`, `/`), "/")
	segments := strings.Split(normalized, "/")
	if len(segments) < 3 || segments[len(segments)-2] != "snapshots" {
		return ""
	}
	c := segments[len(segments)-3]
	if !validPartition(c) {
		return ""
	}
	return c
}

// filterPartitionSnapshots 只保留属于目标分区的快照目录（C8：三方一致的第二道防线——
// snapshot/create 按前缀返回，此处再按基名日期过滤一次）。
func filterPartitionSnapshots(paths []string, partition string) []string {
	var out []string
	for _, p := range paths {
		if snapshotPartition(p) == partition {
			out = append(out, p)
		}
	}
	return out
}

// scanSnapshotDirs 遍历快照目录采集文件清单（C2：唯一合法数据源）。
// 快照目录内容镜像分区结构（indexdb/…、datadb/…，设计文档 §9），
// manifest 相对路径以快照根为基准，恢复时按此整体还原 partitions/<date>/ 结构。
// 返回按路径排序的待上传清单（保证 manifest 确定性）。
func scanSnapshotDirs(dirs []string) ([]entryWithPath, error) {
	scanned := make([]entryWithPath, 0, 64)
	for _, dir := range dirs {
		werr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			sub, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(sub)
			if rel == "" || rel == "." {
				return errs.Untyped(nil, "unexpected snapshot layout under %s", dir)
			}
			sum, err := fileSHA256(path)
			if err != nil {
				return err
			}
			scanned = append(scanned, entryWithPath{
				Entry: backupmeta.Entry{Path: rel, Size: info.Size(), SHA256: sum},
				local: path,
			})
			return nil
		})
		if werr != nil {
			return nil, errs.Untyped(werr, "scan snapshot dir %s", dir)
		}
	}
	sort.Slice(scanned, func(i, k int) bool { return scanned[i].Path < scanned[k].Path })
	return scanned, nil
}

// fileSHA256 流式计算文件 SHA256（分区文件可达 GB 级，禁止整读进内存）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errs.Untyped(err, "open %s", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return "", errs.Untyped(err, "hash %s", path)
	}
	if err := f.Close(); err != nil {
		return "", errs.Untyped(err, "close %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
