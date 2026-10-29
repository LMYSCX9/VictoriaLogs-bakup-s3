// Package report 提供作业摘要的表格与单行 JSON 两种渲染。
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Summary 是各子命令通用的结果摘要（R-OPS-02：9 个基础字段 + op 专属扩展字段用 omitempty）。
type Summary struct {
	Op        string `json:"op"`                  // setup/backup/restore/list/verify/delete/gc
	Status    string `json:"status"`              // success / error / canceled
	Cluster   string `json:"cluster,omitempty"`   //
	Node      string `json:"node,omitempty"`      //
	Partition string `json:"partition,omitempty"` // YYYY-MM-DD (UTC)
	Files     int64  `json:"files,omitempty"`     // 文件数
	Bytes     int64  `json:"bytes,omitempty"`     // 总字节
	ElapsedMS int64  `json:"elapsed_ms"`          // 耗时（毫秒）
	ErrorCode int    `json:"error_code,omitempty"`

	// —— op 专属扩展字段（omitempty：仅对应 op 出现）——
	Partitions       []string `json:"partitions,omitempty"`        // 多分区批量作业（backup/list/delete）
	Skipped          int64    `json:"skipped,omitempty"`           // backup：跳过的已存在对象数（R-BK-05）
	Today            bool     `json:"today,omitempty"`             // backup：含 UTC 今天分区（R-BK-04）
	SnapshotRetained bool     `json:"snapshot_retained,omitempty"` // backup：快照未清理（--keep-snapshot，R-BK-07 提示占用）
	SmokeQuery       string   `json:"smoke_query,omitempty"`       // restore：ok/empty/failed（R-RS-07 固定字段名）
	ReplacedPath     string   `json:"replaced_path,omitempty"`     // restore：旧目录保留路径（R-RS-06）
	CrossNode        bool     `json:"cross_node,omitempty"`        // restore：跨节点恢复标注（R-MF-04）
	Path             string   `json:"path,omitempty"`              // delete：分区绝对路径（§11.3）
	HasBackup        bool     `json:"has_backup,omitempty"`        // delete：是否已有在线备份（§11.3）
	DryRun           bool     `json:"dry_run,omitempty"`           // delete/gc/list：演练模式
	Units            int64    `json:"units,omitempty"`             // gc：删除备份单元数（§11.3）
	Objects          int64    `json:"objects,omitempty"`           // gc：删除对象数（§11.3）
	Leftovers        int64    `json:"leftovers,omitempty"`         // gc：残留对象数（§11.3）
}

// JSON 把摘要渲染为单行 JSON（含末尾换行），满足 R-OPS-02。
func JSON(w io.Writer, s Summary) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

// Table 把摘要渲染为人类可读的 key: value 列表。
func Table(w io.Writer, s Summary) error {
	rows := []struct{ k, v string }{
		{"op", s.Op},
		{"status", s.Status},
	}
	if s.Cluster != "" {
		rows = append(rows, struct{ k, v string }{"cluster", s.Cluster})
	}
	if s.Node != "" {
		rows = append(rows, struct{ k, v string }{"node", s.Node})
	}
	if len(s.Partitions) > 0 {
		rows = append(rows, struct{ k, v string }{"partitions", strings.Join(s.Partitions, ",")})
	}
	if s.Partition != "" {
		rows = append(rows, struct{ k, v string }{"partition", s.Partition})
	}
	if s.Files > 0 {
		rows = append(rows, struct{ k, v string }{"files", fmt.Sprintf("%d", s.Files)})
	}
	if s.Bytes > 0 {
		rows = append(rows, struct{ k, v string }{"bytes", fmt.Sprintf("%d", s.Bytes)})
	}
	if s.Skipped > 0 {
		rows = append(rows, struct{ k, v string }{"skipped", fmt.Sprintf("%d", s.Skipped)})
	}
	if s.Today {
		rows = append(rows, struct{ k, v string }{"today", "true"})
	}
	if s.SnapshotRetained {
		rows = append(rows, struct{ k, v string }{"snapshot_retained", "true（快照保留中，磁盘占用待 --keep-snapshot 生命周期结束后清理）"})
	}
	if s.SmokeQuery != "" {
		rows = append(rows, struct{ k, v string }{"smoke_query", s.SmokeQuery})
	}
	if s.ReplacedPath != "" {
		rows = append(rows, struct{ k, v string }{"replaced_path", s.ReplacedPath})
	}
	if s.CrossNode {
		rows = append(rows, struct{ k, v string }{"cross_node", "true（跨节点恢复）"})
	}
	if s.Path != "" {
		rows = append(rows, struct{ k, v string }{"path", s.Path})
	}
	if s.HasBackup {
		rows = append(rows, struct{ k, v string }{"has_backup", "true"})
	}
	if s.DryRun {
		rows = append(rows, struct{ k, v string }{"dry_run", "true"})
	}
	if s.Units > 0 {
		rows = append(rows, struct{ k, v string }{"units", fmt.Sprintf("%d", s.Units)})
	}
	if s.Objects > 0 {
		rows = append(rows, struct{ k, v string }{"objects", fmt.Sprintf("%d", s.Objects)})
	}
	if s.Leftovers > 0 {
		rows = append(rows, struct{ k, v string }{"leftovers", fmt.Sprintf("%d", s.Leftovers)})
	}
	rows = append(rows, struct{ k, v string }{"elapsed_ms", fmt.Sprintf("%d", s.ElapsedMS)})
	if s.ErrorCode != 0 {
		rows = append(rows, struct{ k, v string }{"error_code", fmt.Sprintf("%d", s.ErrorCode)})
	}

	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "%-18s %s\n", r.k, r.v); err != nil {
			return fmt.Errorf("write summary: %w", err)
		}
	}
	return nil
}
