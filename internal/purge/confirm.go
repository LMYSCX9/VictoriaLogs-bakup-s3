// purge 包的确认提示内容（R-DEL-02、设计文档 §7.5「确认提示」硬要求）。
// cmd 层负责把提示输出到 stderr 并收集用户输入；本包只负责生成提示文本，
// 保证六个字段（分区日期 UTC / 绝对路径 / 规模 / 在线备份 / 影响 / 可恢复性）不缺。
package purge

import (
	"fmt"
	"io"
	"time"
)

// RenderConfirm 输出删除确认提示。输出目标是 io.Writer，由 cmd 决定落到 stderr；
// 不含末尾的「请输入日期串」交互行——那是 cmd 层确认交互的一部分（R-DEL-03）。
func RenderConfirm(w io.Writer, info Info) {
	fmt.Fprintln(w, "即将删除本地分区数据，此操作不可恢复")
	fmt.Fprintf(w, "  分区        : %s (UTC)\n", formatDay(info.Partition))
	fmt.Fprintf(w, "  目录        : %s\n", info.Path)
	fmt.Fprintf(w, "  规模        : %d 个文件 / %s\n", info.Files, formatBytes(info.Bytes))
	backup, recoverability := formatBackup(info)
	fmt.Fprintf(w, "  在线备份    : %s\n", backup)
	fmt.Fprintf(w, "  影响        : 该日日志将立即不可查询；其他日期不受影响；进程不重启\n")
	fmt.Fprintf(w, "  可恢复性    : %s\n", recoverability)
	if info.IsToday {
		fmt.Fprintln(w, "  警告        : 该分区是 UTC 今天，仍有日志写入，删除将造成实时数据缺口")
	}
}

// formatBackup 渲染「在线备份 / 可恢复性」两个字段的内容（R-DEL-02 双文案）。
func formatBackup(info Info) (backup, recoverability string) {
	if info.HasBackup {
		return "有（已提交 " + info.BackupKey + "）",
			"已有已提交备份，删除后可随时 restore " + formatDay(info.Partition) + " 回灌"
	}
	return "无（" + info.UnitPrefix + " 无 manifest）",
		"无备份，删除后无法找回"
}

// formatDay 把分区名 YYYYMMDD 渲染为 YYYY-MM-DD（设计模板的日期展示格式）。
func formatDay(partition string) string {
	t, err := time.Parse("20060102", partition)
	if err != nil {
		return partition
	}
	return t.Format("2006-01-02")
}

// formatBytes 人性化字节（二进制单位，与设计模板「58.3 GiB」一致）。
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
