// purge 包的回滚与启动自检逻辑（设计文档 §7.5 失败表、R-DEL-11）。
// 与主流程分文件（AGENTS §5.3），便于评审时逐条对照失败表：
//
//	失败点 1  detach 失败            → 不回滚（目录未动，分区仍在线）
//	失败点 2  detach 成功、rename 失败 → attachBack 挂回 present（本文件）
//	失败点 3  删除中失败              → 不回滚，残留 .deleting-* + 人工命令
//	失败点 4  复查发现目录仍在        → 退出码 1 + 绝对路径人工指引
package purge

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// attachBack 在「detach 成功、目录未动」的失败点把分区挂回 present
// （§7.5 失败表第 2 行，R-DEL-09）。可重入：失败后可再次调用。
func (j *Job) attachBack(ctx context.Context, partition string) error {
	j.log.Info("attach partition back after failed delete", slog.String("partition", partition))
	if err := j.vl.Attach(ctx, partition); err != nil {
		return errs.Network(err, "attach partition %s", partition)
	}
	return nil
}

// CleanupDeleting 清理 <dataPath>/.deleting-* 残留（R-DEL-11 启动自检，
// 由 cmd 在子命令入口统一调用）。返回清理的条目数；任一条目失败不中断其余条目。
func CleanupDeleting(dataPath string, log *slog.Logger) (int, error) {
	if log == nil {
		log = slog.Default()
	}
	matches, err := filepath.Glob(filepath.Join(dataPath, ".deleting-*"))
	if err != nil {
		return 0, errs.Untyped(err, "glob deleting leftovers under %s", dataPath)
	}
	removed := 0
	var joinErr error
	for _, path := range matches {
		abs := path
		if a, aerr := filepath.Abs(path); aerr == nil {
			abs = a
		}
		// AGENTS §5.3：删除前必须先写日志（含绝对路径）。
		log.Warn("found leftover .deleting-* entry from an interrupted delete, removing it",
			slog.String("path", abs))
		if rerr := os.RemoveAll(path); rerr != nil {
			joinErr = errors.Join(joinErr, errs.Untyped(rerr, "remove leftover %s", path))
			continue
		}
		removed++
	}
	return removed, joinErr
}
