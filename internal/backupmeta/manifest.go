// Package backupmeta 定义 S3 备份单元的 manifest 结构与校验规则（设计文档 §6.2）。
// 本包为纯结构体与校验，禁止 import 任何 IO 包；编解码仅面向内存字节。
// 旧名 internal/manifest，为避免与插件规范的 manifest（自我介绍 JSON）重名，2026-10 更名为 backupmeta。
package backupmeta

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FormatVersion 是本工具写入的 manifest 格式版本（设计文档 §6.1 布局中的 v1）。
// 不兼容变更必须升级此值并更新设计文档 §6（AGENTS.md §7）。
const FormatVersion = 1

// ErrVersionTooNew 表示 manifest 的 format_version 高于本工具支持的版本。
// 编排层必须据此以退出码 5 拒绝恢复/校验，并提示「请升级工具」（R-MF-03）。
var ErrVersionTooNew = errors.New("manifest format_version is newer than supported")

// Entry 描述分区内一个文件的完整性声明；Path 为相对路径，分隔符一律 `/`。
type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest 是 manifest.json 的结构，作为备份单元的唯一提交点（C3，R-MF-01）。
type Manifest struct {
	FormatVersion int       `json:"format_version"`
	Cluster       string    `json:"cluster"`
	Node          string    `json:"node"`
	Partition     string    `json:"partition"` // YYYYMMDD，UTC 语义（C8）
	CreatedAt     time.Time `json:"created_at"`
	SnapshotAt    time.Time `json:"snapshot_at"`
	VLVersion     string    `json:"vl_version"`
	ToolVersion   string    `json:"tool_version"`
	FileCount     int       `json:"file_count"`
	TotalBytes    int64     `json:"total_bytes"`
	Entries       []Entry   `json:"entries"`
}

// CheckVersion 校验格式版本兼容性（R-MF-03）。
// 高于支持版本时返回可被 errors.Is 识别为 ErrVersionTooNew 的错误，
// 供编排层映射为退出码 5 并提示升级工具。
func CheckVersion(v int) error {
	if v > FormatVersion {
		return fmt.Errorf("%w: got %d, supported %d, please upgrade the tool", ErrVersionTooNew, v, FormatVersion)
	}
	if v != FormatVersion {
		return fmt.Errorf("format_version must be %d, got %d", FormatVersion, v)
	}
	return nil
}

// Parse 从字节解析 manifest 并完成全量校验（R-MF-02），
// 保证「解析成功即校验通过」，避免编排层遗漏 Validate。
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest json: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate 校验 manifest 自身完整性：字段齐全、汇总值与 entries 一致、路径安全。
// 不涉及与请求参数的身份比对（cluster/node/partition 由编排层按 R-MF-04 处理）。
func (m *Manifest) Validate() error {
	if err := CheckVersion(m.FormatVersion); err != nil {
		return err
	}
	if m.Cluster == "" {
		return fmt.Errorf("manifest cluster is missing")
	}
	if m.Node == "" {
		return fmt.Errorf("manifest node is missing")
	}
	if !validPartition(m.Partition) {
		return fmt.Errorf("manifest partition %q is not a valid UTC date in YYYYMMDD", m.Partition)
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("manifest created_at is missing")
	}
	if m.SnapshotAt.IsZero() {
		return fmt.Errorf("manifest snapshot_at is missing")
	}
	if m.VLVersion == "" {
		return fmt.Errorf("manifest vl_version is missing")
	}
	if m.ToolVersion == "" {
		return fmt.Errorf("manifest tool_version is missing")
	}

	seen := make(map[string]struct{}, len(m.Entries))
	var sum int64
	for _, e := range m.Entries {
		if !validRelPath(e.Path) {
			return fmt.Errorf("manifest entry path %q is not a safe relative path", e.Path)
		}
		if e.Size < 0 {
			return fmt.Errorf("manifest entry %s has negative size %d", e.Path, e.Size)
		}
		if !validSHA256(e.SHA256) {
			return fmt.Errorf("manifest entry %s has invalid sha256", e.Path)
		}
		if _, dup := seen[e.Path]; dup {
			return fmt.Errorf("manifest has duplicate entry %s", e.Path)
		}
		seen[e.Path] = struct{}{}
		sum += e.Size
	}
	if m.FileCount != len(m.Entries) {
		return fmt.Errorf("manifest file_count %d does not match %d entries", m.FileCount, len(m.Entries))
	}
	if m.TotalBytes != sum {
		return fmt.Errorf("manifest total_bytes %d does not match entries sum %d", m.TotalBytes, sum)
	}
	return nil
}

// validPartition 要求恰好 8 位数字且为真实存在的日历日期。
// 月份/日期越界（如 20261301）会被 time.Parse 拒绝；时区固定 UTC（C8）。
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

// validRelPath 校验分区内相对路径：禁止绝对路径、反斜杠、盘符冒号、
// `..` 穿越段、空段与 `.` 段，防止恢复路径穿越（设计文档 §13 路径安全）。
func validRelPath(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.ContainsAny(p, `\:`) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".", "..":
			return false
		}
	}
	return true
}

// validSHA256 要求 64 位十六进制字符（32 字节摘要）。
func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32
}
