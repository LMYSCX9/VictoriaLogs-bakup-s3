package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/vlogs-backup/vlogs-backup/internal/plugin"
)

// runDescribe 实现 `<bin> describe`（manifest-spec §2）：
// 向 stdout 输出完整插件 manifest JSON；stderr 写日志（本实现无日志）。
// 退出码：0 成功；非 0 视为无效插件（handshake-protocol §2）。
func runDescribe(stdout io.Writer, stderr io.Writer) int {
	m, err := buildPluginManifest()
	if err != nil {
		fmt.Fprintf(stderr, "build plugin manifest: %v\n", err)
		return plugin.ExitTaskGeneralCrash
	}
	if err := m.Validate(); err != nil {
		fmt.Fprintf(stderr, "manifest validation: %v\n", err)
		return plugin.ExitTaskGeneralCrash
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "marshal manifest: %v\n", err)
		return plugin.ExitTaskGeneralCrash
	}
	fmt.Fprintln(stdout, string(b))
	return plugin.ExitTaskOK
}

// buildPluginManifest 构造完整的插件 manifest 声明。
func buildPluginManifest() (*plugin.Manifest, error) {
	selfPath, err := os.Executable()
	if err != nil {
		// 回退：从 args[0] 取
		if len(os.Args) > 0 {
			selfPath = os.Args[0]
		} else {
			selfPath = "vlogs-backup"
		}
	}

	m := &plugin.Manifest{
		ManifestVersion: plugin.ManifestVersion,
		Name:            "vlogs-backup",
		DisplayName:     "VictoriaLogs S3 备份/恢复",
		Version:         currentVersion().Version,
		APIVersion:      plugin.APIVersion,
		Description:     "在 VictoriaLogs 不停机前提下，把 UTC 按天分区的历史日志备份到 S3 兼容对象存储；支持按指定日期在线回灌、本地分区删除与 S3 清单校验",
		Modes:           []plugin.Mode{plugin.ModeTask},
		Binary: plugin.Binary{
			Path: filepath.Base(selfPath),
			Args: []string{},
		},
		Tasks:       buildTaskDecls(),
		Permissions: buildPermissions(),
		Config:      buildConfigSchema(),
	}
	return m, nil
}

// buildTaskDecls 返回 7 个 task 的声明（对应 CLI 的 7 个子命令）。
func buildTaskDecls() []plugin.Task {
	return []plugin.Task{
		{
			Name:        "backup",
			Title:       "备份指定日期分区",
			Description: "把一个或多个 UTC 日期分区备份到 S3；支持单日、日期区间与最近 N 天三种输入方式；逐天独立执行，单天失败不影响其余天",
			Input: jsonSchemaObject(map[string]any{
				"date":          map[string]any{"type": "string", "description": "备份单日分区 YYYY-MM-DD（默认 UTC 昨天）"},
				"from":          map[string]any{"type": "string", "description": "批量起始日期 YYYY-MM-DD（含端点），需与 to 成对"},
				"to":            map[string]any{"type": "string", "description": "批量结束日期 YYYY-MM-DD（含端点），需与 from 成对"},
				"latest":        map[string]any{"type": "integer", "minimum": 1, "description": "备份最近 N 天（含 UTC 今天）"},
				"if_exists":     map[string]any{"type": "string", "enum": []string{"skip", "replace"}, "default": "skip", "description": "目标日期已有已提交备份时的处理策略"},
				"keep_snapshot": map[string]any{"type": "boolean", "default": false, "description": "保留快照用于排障（占用磁盘，需人工清理）"},
				"dry_run":       map[string]any{"type": "boolean", "default": false, "description": "只演练：探测 VL 能力并列出计划分区，不上传 S3"},
			}),
			Timeout: 3600, // 单日十几 GB 备份保守按 1 小时
		},
		{
			Name:        "restore",
			Title:       "在线恢复指定日期分区",
			Description: "在不重启 VL 的前提下，把指定日期的备份单元回灌到本地 partitions/；走 detach → 下载校验 → attach 原子路径；超出 retention 窗口（默认 181 天）直接拒绝",
			Input: jsonSchemaObject(map[string]any{
				"date":                map[string]any{"type": "string", "description": "恢复分区 YYYY-MM-DD（默认 UTC 昨天）"},
				"on_existing":         map[string]any{"type": "string", "enum": []string{"keep", "delete"}, "default": "keep", "description": "目标分区已存在时：keep 保留旧目录为 .replaced-*，delete 直接删除"},
				"deep":                map[string]any{"type": "boolean", "default": false, "description": "下载后逐文件比对 sha256（默认只校验 size）"},
				"force_today":         map[string]any{"type": "boolean", "default": false, "description": "允许恢复 UTC 今天分区（造成实时写入缺口）"},
				"allow_node_mismatch": map[string]any{"type": "boolean", "default": false, "description": "放行 manifest 中 cluster/node 与当前节点不一致的备份（跨节点恢复）"},
				"dry_run":             map[string]any{"type": "boolean", "default": false, "description": "只演练：探测 VL 能力并打印计划，不下载不替换"},
				// yes 由 task 模式自动注入，仍允许调用方显式覆盖
				"yes": map[string]any{"type": "boolean", "default": true, "description": "跳过交互确认（task 模式默认 true）"},
			}),
			Timeout: 1800,
		},
		{
			Name:        "list",
			Title:       "列出已提交备份单元",
			Description: "只读扫描 S3，列出本节点所有含已提交 manifest 的分区；无 manifest 的残缺前缀不展示",
			Input: jsonSchemaObject(map[string]any{
				"from": map[string]any{"type": "string", "description": "起始日期 YYYY-MM-DD（含端点），需与 to 成对"},
				"to":   map[string]any{"type": "string", "description": "结束日期 YYYY-MM-DD（含端点），需与 from 成对"},
			}),
			Timeout: 120,
		},
		{
			Name:        "verify",
			Title:       "校验单个备份单元",
			Description: "校验指定日期备份单元的完整性：manifest 存在性 → 逐对象 HEAD 比对 size；--deep 时再下载全量比对 sha256",
			Input: jsonSchemaObject(map[string]any{
				"date": map[string]any{"type": "string", "description": "校验分区 YYYY-MM-DD（默认 UTC 昨天）"},
				"deep": map[string]any{"type": "boolean", "default": false, "description": "逐对象全量下载比对 sha256"},
			}),
			Timeout: 1800,
		},
		{
			Name:        "delete",
			Title:       "删除本地指定日期分区",
			Description: "在不重启 VL 的前提下删除本地 partitions/<date> 整个目录；必须先 detach、再 rename 出 partitions/ 后递归删除；无备份时默认拒绝；不触碰 S3",
			Input: jsonSchemaObject(map[string]any{
				"date":            map[string]any{"type": "string", "description": "删除分区 YYYY-MM-DD（默认 UTC 昨天）"},
				"from":            map[string]any{"type": "string", "description": "批量起始日期 YYYY-MM-DD（含端点），需与 to 成对"},
				"to":              map[string]any{"type": "string", "description": "批量结束日期 YYYY-MM-DD（含端点），需与 from 成对"},
				"force_today":     map[string]any{"type": "boolean", "default": false, "description": "允许删除 UTC 今天分区（造成实时日志缺口）"},
				"force_no_backup": map[string]any{"type": "boolean", "default": false, "description": "允许删除 S3 上无已提交备份的分区（不可恢复）"},
				"dry_run":         map[string]any{"type": "boolean", "default": false, "description": "只演练：打印将删除的分区统计，不执行删除"},
				"yes":             map[string]any{"type": "boolean", "default": true, "description": "跳过交互确认（task 模式默认 true）"},
			}),
			Timeout: 600,
		},
		{
			Name:        "gc",
			Title:       "清理 S3 上未完成备份前缀",
			Description: "清理 S3 上无 manifest 且最后修改时间早于 stale-after（默认 24h）的未完成备份前缀；先删 manifest 再删数据对象；默认 dry-run，需显式 yes 才执行",
			Input: jsonSchemaObject(map[string]any{
				// CLI --stale-after 是 Go duration 字符串（如 24h），此处保持同型，避免翻译后 flag 解析失败
				"stale_after": map[string]any{"type": "string", "default": "24h", "description": "无 manifest 前缀的陈旧阈值，Go duration 写法（如 24h、1h）"},
				"yes":         map[string]any{"type": "boolean", "default": false, "description": "执行删除；不指定时只打印将删除的对象清单"},
			}),
			Timeout: 300,
		},
	}
}

// jsonSchemaObject 构造 JSON Schema draft 2020-12 的 object 类型 raw JSON。
func jsonSchemaObject(props map[string]any) json.RawMessage {
	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	b, _ := json.Marshal(schema)
	return b
}

// buildPermissions 返回插件声明的权限集合（manifest-spec §3.5 声明制权限）。
func buildPermissions() []string {
	return []string{
		// 自身 KV 存储（底座分配的插件数据目录）
		"storage.plugin-scope:*",
		// 出站 HTTP：VL 端点分区管理与快照、S3 API、以及探测用
		"http.call:*",
	}
}

// buildConfigSchema 返回配置项 schema（对应 config.Config 字段，manifest-spec §3.6）。
func buildConfigSchema() map[string]plugin.ConfigItem {
	return map[string]plugin.ConfigItem{
		"vl_url": {
			Type:        "string",
			Title:       "VL 地址",
			Description: "VictoriaLogs 分区管理与快照端点 URL，含协议与端口",
			Required:    true,
		},
		"vl_auth_mode": {
			Type:        "enum",
			Title:       "VL 鉴权模式",
			Description: "生产 VL 前置 vmauth；none 为直连",
			Default:     "none",
			Enum:        []string{"none", "basic", "bearer"},
		},
		"vl_username": {
			Type:        "string",
			Title:       "VL 用户名",
			Description: "vl_auth_mode=basic 时必填",
		},
		"vl_password": {
			Type:        "string",
			Title:       "VL 密码",
			Description: "vl_auth_mode=basic 时必填",
			Secret:      true,
		},
		"vl_token": {
			Type:        "string",
			Title:       "VL Bearer 令牌",
			Description: "vl_auth_mode=bearer 时必填",
			Secret:      true,
		},
		"data_path": {
			Type:        "string",
			Title:       "数据目录",
			Description: "VL 数据根目录（含 partitions/、snapshots/），用于本地分区 delete 操作",
			Required:    true,
		},
		"cluster": {
			Type:        "string",
			Title:       "集群标识",
			Description: "写入 manifest 与 S3 键；按 UTC 维度恢复时用于身份校验；定好后不要再改",
			Default:     "default",
		},
		"node": {
			Type:        "string",
			Title:       "节点标识",
			Description: "写入 manifest 与 S3 键；与 cluster 一起标识本节点身份；同 cluster 下唯一，定好后不要再改",
			Default:     "default",
		},
		"s3_endpoint": {
			Type:     "string",
			Title:    "S3 Endpoint",
			Required: true,
		},
		"s3_region": {
			Type:  "string",
			Title: "S3 Region",
		},
		"s3_bucket": {
			Type:     "string",
			Title:    "S3 Bucket",
			Required: true,
		},
		"s3_prefix": {
			Type:    "string",
			Title:   "S3 Prefix",
			Default: "vlogs",
		},
		"s3_path_style": {
			Type:    "bool",
			Title:   "S3 Path-Style",
			Default: true,
		},
		"s3_access_key": {
			Type:     "string",
			Title:    "S3 Access Key",
			Required: true,
			Secret:   true,
		},
		"s3_secret_key": {
			Type:     "string",
			Title:    "S3 Secret Key",
			Required: true,
			Secret:   true,
		},
		"vl_retention_days": {
			Type:        "int",
			Title:       "VL 保留窗口（天）",
			Description: "必须 ≥ 181，与 VL -retentionPeriod 对齐；restore 超出此窗口直接拒绝",
			Default:     181,
		},
		"concurrency": {
			Type:        "int",
			Title:       "并发 Worker 数",
			Description: "备份/恢复/校验并发度",
			Default:     8,
		},
		"job_timeout": {
			Type:        "string",
			Title:       "单作业超时",
			Description: "Go duration 写法（如 30m）；\"0\" 表示不限整体时长；进程级 ctx 超时，超时后当前分区走取消路径",
			Default:     "0",
		},
		"orphan_snapshot_age": {
			Type:        "string",
			Title:       "孤立快照阈值",
			Description: "Go duration 写法（如 2h）；启动时清理存在超过此时长的无锁快照",
			Default:     "2h",
		},
		"s3_multipart_threshold": {
			Type:        "int",
			Title:       "S3 分段上传阈值（字节）",
			Description: "超过此大小时使用分段上传",
			Default:     67108864,
		},
		"s3_multipart_part_size": {
			Type:        "int",
			Title:       "S3 分段大小（字节）",
			Description: "每个分段的大小",
			Default:     8388608,
		},
		"disk_check_factor": {
			Type:        "string",
			Title:       "磁盘预检系数",
			Description: "浮点数字符串（如 \"1.1\"）；恢复前要求剩余空间 ≥ 分区大小 × 此系数",
			Default:     "1.1",
		},
		"io_timeout": {
			Type:        "string",
			Title:       "IO 超时",
			Description: "Go duration 写法（如 10m）；单次网络 IO 超时",
			Default:     "10m",
		},
	}
}
