package main

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/plugin"
)

// buildTestManifest 返回 buildPluginManifest 的结果（跳过错误即失败）。
func buildTestManifest(t *testing.T) *plugin.Manifest {
	t.Helper()
	m, err := buildPluginManifest()
	if err != nil {
		t.Fatalf("buildPluginManifest: %v", err)
	}
	return m
}

// TestDescribeManifestValidates 保证 describe 输出始终通过 manifest v1 校验（R-PL-01）。
func TestDescribeManifestValidates(t *testing.T) {
	m := buildTestManifest(t)
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ManifestVersion != plugin.ManifestVersion {
		t.Errorf("manifestVersion = %d, want %d", m.ManifestVersion, plugin.ManifestVersion)
	}
	modes := map[plugin.Mode]bool{}
	for _, mo := range m.Modes {
		modes[mo] = true
	}
	if !modes[plugin.ModeTask] || modes[plugin.ModeDaemon] {
		t.Errorf("modes = %v, want [task] only", m.Modes)
	}
}

// TestDescribeTaskSet 保证 describe 声明的 task 与可执行入口一致（R-PL-01/R-PL-05）：
// 6 个可 run 的 task 必须都在 manifest 中；setup/version 不得出现。
func TestDescribeTaskSet(t *testing.T) {
	m := buildTestManifest(t)
	got := map[string]bool{}
	for _, task := range m.Tasks {
		got[task.Name] = true
		if task.Timeout <= 0 {
			t.Errorf("task %s has no timeout", task.Name)
		}
		if len(task.Input) == 0 {
			t.Errorf("task %s has empty input schema", task.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(task.Input, &schema); err != nil {
			t.Errorf("task %s input is not valid json: %v", task.Name, err)
		}
	}
	for _, want := range []string{"backup", "restore", "list", "verify", "delete", "gc"} {
		if !got[want] {
			t.Errorf("task %q missing from manifest", want)
		}
	}
	for _, banned := range []string{"setup", "version"} {
		if got[banned] {
			t.Errorf("task %q must not be declared (no task form)", banned)
		}
	}
}

// TestDescribeConfigDefaultsMatchCode 是本次审计发现的漂移问题的回归防线：
// describe config schema 的默认值/类型必须与 config.Default() 完全一致，
// 否则底座渲染的默认值与插件真实行为不符（R-PL-01）。
// 新增配置项时：先在 config.Default() 加默认值，再在 buildConfigSchema 加 schema，本测试兜底。
func TestDescribeConfigDefaultsMatchCode(t *testing.T) {
	m := buildTestManifest(t)
	cfg := config.Default()
	items := m.Config
	if len(items) == 0 {
		t.Fatal("describe config schema is empty")
	}

	// stringWithDuration 断言 string 类型的 default 解析为 duration 后与 Config 字段一致。
	stringWithDuration := func(key string, want time.Duration) {
		t.Helper()
		item, ok := items[key]
		if !ok {
			t.Fatalf("config %q missing from describe schema", key)
		}
		if item.Type != "string" {
			t.Errorf("config %q type = %q, want string (duration)", key, item.Type)
		}
		s, ok := item.Default.(string)
		if !ok {
			t.Fatalf("config %q default is not a string: %v", key, item.Default)
		}
		got, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("config %q default %q is not a valid duration: %v", key, s, err)
		}
		if got != want {
			t.Errorf("config %q default = %q (%s), code default is %s", key, s, got, want)
		}
	}

	// stringDefault 断言 string 类型 default 与期望一致。
	stringDefault := func(key, want string) {
		t.Helper()
		item, ok := items[key]
		if !ok {
			t.Fatalf("config %q missing from describe schema", key)
		}
		if item.Type != "string" {
			t.Errorf("config %q type = %q, want string", key, item.Type)
		}
		if item.Default != want {
			t.Errorf("config %q default = %v, want %q", key, item.Default, want)
		}
	}

	// intDefault 断言 int 类型 default 与 Config 字段一致。
	intDefault := func(key string, want int64) {
		t.Helper()
		item, ok := items[key]
		if !ok {
			t.Fatalf("config %q missing from describe schema", key)
		}
		if item.Type != "int" {
			t.Errorf("config %q type = %q, want int", key, item.Type)
		}
		n, ok := item.Default.(int)
		if !ok {
			t.Fatalf("config %q default is not an int: %v", key, item.Default)
		}
		if int64(n) != want {
			t.Errorf("config %q default = %d, code default is %d", key, n, want)
		}
	}

	// boolDefault 断言 bool 类型 default 与 Config 字段一致。
	boolDefault := func(key string, want bool) {
		t.Helper()
		item, ok := items[key]
		if !ok {
			t.Fatalf("config %q missing from describe schema", key)
		}
		if item.Type != "bool" {
			t.Errorf("config %q type = %q, want bool", key, item.Type)
		}
		if item.Default != want {
			t.Errorf("config %q default = %v, code default is %v", key, item.Default, want)
		}
	}

	// 带默认值的配置项逐项对齐 config.Default()。
	stringDefault("cluster", cfg.Cluster)
	stringDefault("node", cfg.Node)
	stringDefault("s3_prefix", cfg.S3Prefix)
	boolDefault("s3_path_style", cfg.S3PathStyle)
	intDefault("concurrency", int64(cfg.Concurrency))
	intDefault("s3_multipart_threshold", cfg.S3MultipartThreshold)
	intDefault("s3_multipart_part_size", cfg.S3MultipartPartSize)
	intDefault("vl_retention_days", int64(cfg.VLRetentionDays))
	stringWithDuration("io_timeout", cfg.IOTimeout)
	stringWithDuration("job_timeout", cfg.JobTimeout)
	stringWithDuration("orphan_snapshot_age", cfg.OrphanSnapshotAge)

	// disk_check_factor 是浮点，manifest v1 无 number 类型，以字符串承载；
	// 断言其解析值与 Config 字段一致。
	item, ok := items["disk_check_factor"]
	if !ok {
		t.Fatal("config disk_check_factor missing from describe schema")
	}
	s, ok := item.Default.(string)
	if !ok {
		t.Fatalf("disk_check_factor default is not a string: %v", item.Default)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("disk_check_factor default %q is not a float: %v", s, err)
	}
	if f != cfg.DiskCheckFactor {
		t.Errorf("disk_check_factor default = %s, code default is %g", s, cfg.DiskCheckFactor)
	}

	// vl_auth_mode 是 enum：default 必须在枚举内且与代码默认一致。
	item, ok = items["vl_auth_mode"]
	if !ok {
		t.Fatal("config vl_auth_mode missing from describe schema")
	}
	if item.Type != "enum" {
		t.Errorf("vl_auth_mode type = %q, want enum", item.Type)
	}
	inEnum := false
	for _, e := range item.Enum {
		if e == cfg.VLAuthMode {
			inEnum = true
		}
	}
	if !inEnum {
		t.Errorf("vl_auth_mode default %q not in enum %v", cfg.VLAuthMode, item.Enum)
	}
	if item.Default != cfg.VLAuthMode {
		t.Errorf("vl_auth_mode default = %v, code default is %q", item.Default, cfg.VLAuthMode)
	}
}

// TestDescribeRequiredAndSecretFields 校验 required / secret 标注（R-PL-01、C9）。
func TestDescribeRequiredAndSecretFields(t *testing.T) {
	m := buildTestManifest(t)

	for _, key := range []string{"vl_url", "data_path", "s3_endpoint", "s3_bucket", "s3_access_key", "s3_secret_key"} {
		item, ok := m.Config[key]
		if !ok {
			t.Fatalf("config %q missing", key)
		}
		if !item.Required {
			t.Errorf("config %q must be required", key)
		}
	}

	for _, key := range []string{"s3_access_key", "s3_secret_key", "vl_password", "vl_token"} {
		item, ok := m.Config[key]
		if !ok {
			t.Fatalf("config %q missing", key)
		}
		if !item.Secret {
			t.Errorf("config %q must be marked secret", key)
		}
		// C9：secret 项不得携带默认值（防真实凭据经 describe 泄漏）。
		if item.Default != nil {
			t.Errorf("secret config %q must not carry a default, got %v", key, item.Default)
		}
	}
}
