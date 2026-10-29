// 回滚逻辑与主流程分文件（AGENTS §5.3），逐条对照设计文档 §7.3 回滚矩阵：
//
//	② detach 后旧目录 rename 前      → attach 原目录
//	③ rename 后 attach 前            → 旧目录改回原名 → attach（无旧目录则落位新目录 → attach）
//	④ attach 失败                    → 重试一次 → 仍失败停现场 + 人工命令（主流程实现，见 restore.go）
//	⑤ 回滚自身失败                   → 立即终止 + 现场状态 + 人工命令，退出码 1（finishRollback 实现）
//
// 原则：回滚不追求恢复原样，追求不存在半可用状态（C5）——
// 分区要么回到 present（原目录或新目录 attach），要么明确停在「分区缺失 + 人工指引」。
package restore

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// restoreState 记录主流程已推进到的现场，供 rollbackToPresent 判定回滚路径。
// 字段只能由主流程（restore.go）与回滚自身更新，保持与真实磁盘/VL 状态同步。
type restoreState struct {
	partition string
	dataPath  string
	target    string // partitions/<partition> 绝对路径

	tmpDir string // 下载临时目录；rename 成功后置空

	detached bool // 已成功 detach
	oldMoved bool // 旧目录已 rename 到 oldPath（keep 策略）
	oldGone  bool // 旧目录已删除（delete 策略）
	newPath  bool // 新目录已 rename 到 target

	oldPath    string // 旧目录当前位置（oldMoved 时非空）
	orphanPath string // 回滚中挪出的新目录位置（诊断用）
}

// summary 返回单行现场状态，用于人工指引（矩阵行⑤）。
func (st *restoreState) summary() string {
	s := "detached=" + strconv.FormatBool(st.detached) +
		" old_moved=" + strconv.FormatBool(st.oldMoved) +
		" old_gone=" + strconv.FormatBool(st.oldGone) +
		" new_in_place=" + strconv.FormatBool(st.newPath)
	if st.oldPath != "" {
		s += " old_dir=" + st.oldPath
	}
	if st.orphanPath != "" {
		s += " orphan_dir=" + st.orphanPath
	}
	return s
}

// oldDirHint 返回旧目录当前路径提示；无旧目录时返回 "(none)"。
func (st *restoreState) oldDirHint() string {
	if st.oldPath != "" {
		return st.oldPath
	}
	return "(none)"
}

// rollbackToPresent 把分区退回 present 态。可重入：每步基于 state 标志执行，
// 已完成的步骤自动跳过。任一步失败立即返回，不继续后续步骤（防止覆盖现场）。
func (j *Job) rollbackToPresent(ctx context.Context, st *restoreState) error {
	var joined error

	// 矩阵行③a：新目录已落位且旧目录在 replaced —— 先把新目录挪去孤儿位，
	// 再让旧目录回原位；孤儿目录保留现场，不删除。
	if st.newPath && st.oldMoved {
		orphan := filepath.Join(st.dataPath, st.partition+".orphan-"+strconv.FormatInt(time.Now().UnixNano(), 10))
		j.log.Info("rollback: move new dir out of place",
			slog.String("from", st.target), slog.String("to", orphan))
		if err := j.rename(st.target, orphan); err != nil {
			return errors.Join(joined, errs.Untyped(err, "rollback rename %s to %s", st.target, orphan))
		}
		st.orphanPath = orphan
		st.newPath = false
	}

	// 矩阵行②：旧目录已移走但新目录未落位（或刚挪走）——旧目录 rename 回原位。
	// target 必须不存在，拒绝覆盖（失败即终止，矩阵行⑤）。
	if !st.newPath && st.oldMoved {
		if err := ensureAbsent(st.target); err != nil {
			return errors.Join(joined, err)
		}
		j.log.Info("rollback: restore old dir",
			slog.String("from", st.oldPath), slog.String("to", st.target))
		if err := j.rename(st.oldPath, st.target); err != nil {
			return errors.Join(joined, errs.Untyped(err, "rollback rename %s to %s", st.oldPath, st.target))
		}
		st.oldMoved, st.oldPath = false, ""
		st.oldGone = false
	}

	// 矩阵行③b：旧目录已删、新目录未落位 —— 尽力把临时目录落位，
	// 成功则分区有完整新数据可 attach；失败则停「分区缺失 + 人工指引」（C5 允许的停点）。
	if !st.newPath && st.oldGone && st.tmpDir != "" {
		if err := ensureAbsent(st.target); err != nil {
			return errors.Join(joined, err)
		}
		j.log.Info("rollback: move downloaded dir into place",
			slog.String("from", st.tmpDir), slog.String("to", st.target))
		if err := j.rename(st.tmpDir, st.target); err != nil {
			joined = errors.Join(joined, errs.Untyped(err, "rollback rename %s to %s", st.tmpDir, st.target))
			return errors.Join(joined, errs.Untyped(nil,
				"partition %s left missing; downloaded data remains at %s; manual: rename it to %s then attach",
				st.partition, st.tmpDir, st.target))
		}
		st.newPath, st.tmpDir = true, ""
	}

	// 分区曾被 detach（或新目录刚落位）→ attach 使分区回 present。
	if st.detached || st.newPath {
		j.log.Info("rollback: attach partition", slog.String("partition", st.partition))
		if err := j.vl.Attach(ctx, st.partition); err != nil {
			joined = errors.Join(joined, errs.Untyped(err, "rollback attach partition %s", st.partition))
		}
	}
	return joined
}

// ensureAbsent 校验路径不存在（供回滚 rename 前防覆盖检查）。
func ensureAbsent(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return errs.Untyped(nil, "rollback: %s already exists, refusing to overwrite", path)
	}
	if os.IsNotExist(err) {
		return nil
	}
	return errs.Untyped(err, "rollback: lstat %s", path)
}
