// Package config 负责配置的默认值、文件、环境变量三层合并与校验。
// 优先级：flag（由 cmd 层赋值覆盖）> VLB_* 环境变量 > 配置文件 > 默认值。
package config

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// VL 分区管理端点鉴权模式（vl_auth_mode），
// 由访问层按标准 Authorization 请求头动态构造，不硬编码。
const (
	AuthModeNone   = "none"
	AuthModeBasic  = "basic"
	AuthModeBearer = "bearer"
)

// Config 是工具的全量配置。
type Config struct {
	// VictoriaLogs
	VLURL string // 如 http://127.0.0.1:9428
	// 经 vmauth 代理访问分区管理端点的鉴权模式：none / basic / bearer。
	VLAuthMode string
	VLUsername string // basic 模式用户名，来自 credentials 文件或 VLB_VL_USERNAME
	VLPassword string // basic 模式密码（敏感），来自 credentials 文件或 VLB_VL_PASSWORD
	VLToken    string // bearer 模式令牌（敏感），来自 credentials 文件或 VLB_VL_TOKEN
	// 与 VL -retentionPeriod 对齐的保留窗口天数（UTC），
	// restore 以此前置校验分区未超出窗口（设计文档 §7.2）。
	VLRetentionDays int

	// 本地存储
	DataPath string // VL --storageDataPath
	Cluster  string
	Node     string

	// S3
	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3Prefix    string
	S3PathStyle bool
	S3AccessKey string // 来自 credentials 文件或 VLB_S3_ACCESS_KEY
	S3SecretKey string // 来自 credentials 文件或 VLB_S3_SECRET_KEY
	// 分段上传阈值与分片大小（字节）；Q2：阈值不写死，全部可配置。
	S3MultipartThreshold int64
	S3MultipartPartSize  int64

	// 作业参数
	Concurrency int
	IOTimeout   time.Duration
	JobTimeout  time.Duration
	// 恢复磁盘预检因子：要求可用空间 ≥ 分区大小 × 该因子（§7.2）。
	DiskCheckFactor float64
	// 孤立快照年龄阈值（§7.6）：超过该时长且未持锁才在启动期清理。
	OrphanSnapshotAge time.Duration

	// 来源
	ConfigPath string // 生效的配置文件路径（空 = 未使用文件）
}

// tomlFile 是 config.toml 的落盘结构，只承载非敏感项。
type tomlFile struct {
	VLURL                string  `toml:"vl_url"`
	VLAuthMode           string  `toml:"vl_auth_mode"`
	VLRetentionDays      int     `toml:"vl_retention_days"`
	DataPath             string  `toml:"data_path"`
	Cluster              string  `toml:"cluster"`
	Node                 string  `toml:"node"`
	S3Endpoint           string  `toml:"s3_endpoint"`
	S3Region             string  `toml:"s3_region"`
	S3Bucket             string  `toml:"s3_bucket"`
	S3Prefix             string  `toml:"s3_prefix"`
	S3PathStyle          bool    `toml:"s3_path_style"`
	S3MultipartThreshold int64   `toml:"s3_multipart_threshold"` // 字节
	S3MultipartPartSize  int64   `toml:"s3_multipart_part_size"` // 字节
	Concurrency          int     `toml:"concurrency"`
	IOTimeout            string  `toml:"io_timeout"`  // Go duration 字符串，如 10m
	JobTimeout           string  `toml:"job_timeout"` // 0 表示不限
	DiskCheckFactor      float64 `toml:"disk_check_factor"`
	OrphanSnapshotAge    string  `toml:"orphan_snapshot_age"` // Go duration 字符串，如 2h
}

// credentialsFile 是 credentials 文件的落盘结构，权限必须 0600。
type credentialsFile struct {
	S3AccessKey string `toml:"s3_access_key"`
	S3SecretKey string `toml:"s3_secret_key"`
	VLUsername  string `toml:"vl_username"`
	VLPassword  string `toml:"vl_password"`
	VLToken     string `toml:"vl_token"`
}

// Default 返回内置默认值。
func Default() *Config {
	return &Config{
		Cluster:         "default",
		Node:            "default",
		S3Prefix:        "vlogs",
		S3PathStyle:     true,
		Concurrency:     8,
		IOTimeout:       10 * time.Minute,
		VLAuthMode:      AuthModeNone,
		VLRetentionDays: 181,

		S3MultipartThreshold: int64(64) << 20, // 64MiB
		S3MultipartPartSize:  int64(8) << 20,  // 8MiB
		DiskCheckFactor:      1.1,
		OrphanSnapshotAge:    2 * time.Hour,
	}
}

// Load 按 默认值 → 配置文件（含同目录 credentials）→ 环境变量 的顺序装配配置。
// configPath 为空时跳过文件层，仅使用默认值与环境变量。
func Load(ctx context.Context, configPath string) (*Config, error) {
	cfg := Default()

	if configPath != "" {
		if err := loadFile(ctx, cfg, configPath); err != nil {
			return nil, err
		}
		if err := loadCredentials(ctx, cfg, filepath.Join(filepath.Dir(configPath), "credentials")); err != nil {
			return nil, err
		}
	}

	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadFile(ctx context.Context, cfg *Config, path string) error {
	var f tomlFile
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		return errs.Usagef(err, "parse config file %s", path)
	}

	cfg.ConfigPath = path
	if f.VLURL != "" {
		cfg.VLURL = f.VLURL
	}
	if md.IsDefined("vl_auth_mode") {
		cfg.VLAuthMode = f.VLAuthMode
	}
	if md.IsDefined("vl_retention_days") {
		cfg.VLRetentionDays = f.VLRetentionDays
	}
	if f.DataPath != "" {
		cfg.DataPath = f.DataPath
	}
	if f.Cluster != "" {
		cfg.Cluster = f.Cluster
	}
	if f.Node != "" {
		cfg.Node = f.Node
	}
	if f.S3Endpoint != "" {
		cfg.S3Endpoint = f.S3Endpoint
	}
	if f.S3Region != "" {
		cfg.S3Region = f.S3Region
	}
	if f.S3Bucket != "" {
		cfg.S3Bucket = f.S3Bucket
	}
	if f.S3Prefix != "" {
		cfg.S3Prefix = f.S3Prefix
	}
	if md.IsDefined("s3_path_style") {
		cfg.S3PathStyle = f.S3PathStyle
	}
	if md.IsDefined("concurrency") {
		cfg.Concurrency = f.Concurrency
	}
	if md.IsDefined("s3_multipart_threshold") {
		cfg.S3MultipartThreshold = f.S3MultipartThreshold
	}
	if md.IsDefined("s3_multipart_part_size") {
		cfg.S3MultipartPartSize = f.S3MultipartPartSize
	}
	if md.IsDefined("disk_check_factor") {
		cfg.DiskCheckFactor = f.DiskCheckFactor
	}

	var perr error
	if cfg.IOTimeout, perr = parseDuration(f.IOTimeout, cfg.IOTimeout); perr != nil {
		return errs.Usagef(perr, "config io_timeout")
	}
	if cfg.JobTimeout, perr = parseDuration(f.JobTimeout, cfg.JobTimeout); perr != nil {
		return errs.Usagef(perr, "config job_timeout")
	}
	if cfg.OrphanSnapshotAge, perr = parseDuration(f.OrphanSnapshotAge, cfg.OrphanSnapshotAge); perr != nil {
		return errs.Usagef(perr, "config orphan_snapshot_age")
	}
	return nil
}

// parseDuration 解析 duration 字符串；空串返回 fallback，"0" 返回 0。
func parseDuration(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", s, err)
	}
	return d, nil
}

func loadCredentials(ctx context.Context, cfg *Config, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // credentials 文件可选，凭据可来自 VLB_* 环境变量
		}
		return errs.Usagef(err, "read credentials file %s", path)
	}

	var c credentialsFile
	if err := toml.Unmarshal(b, &c); err != nil {
		return errs.Usagef(err, "parse credentials file %s", path)
	}
	if c.S3AccessKey != "" {
		cfg.S3AccessKey = c.S3AccessKey
	}
	if c.S3SecretKey != "" {
		cfg.S3SecretKey = c.S3SecretKey
	}
	if c.VLUsername != "" {
		cfg.VLUsername = c.VLUsername
	}
	if c.VLPassword != "" {
		cfg.VLPassword = c.VLPassword
	}
	if c.VLToken != "" {
		cfg.VLToken = c.VLToken
	}
	return nil
}

// envEntry 描述一个环境变量到字段的映射与解析方式。
type envEntry struct {
	key string
	set func(cfg *Config, raw string) error
}

var envEntries = []envEntry{
	{"VLB_VL_URL", func(c *Config, v string) error { c.VLURL = v; return nil }},
	{"VLB_VL_AUTH_MODE", func(c *Config, v string) error { c.VLAuthMode = v; return nil }},
	{"VLB_VL_USERNAME", func(c *Config, v string) error { c.VLUsername = v; return nil }},
	{"VLB_VL_PASSWORD", func(c *Config, v string) error { c.VLPassword = v; return nil }},
	{"VLB_VL_TOKEN", func(c *Config, v string) error { c.VLToken = v; return nil }},
	{"VLB_VL_RETENTION_DAYS", setInt(func(c *Config, n int) { c.VLRetentionDays = n })},
	{"VLB_DATA_PATH", func(c *Config, v string) error { c.DataPath = v; return nil }},
	{"VLB_CLUSTER", func(c *Config, v string) error { c.Cluster = v; return nil }},
	{"VLB_NODE", func(c *Config, v string) error { c.Node = v; return nil }},
	{"VLB_S3_ENDPOINT", func(c *Config, v string) error { c.S3Endpoint = v; return nil }},
	{"VLB_S3_REGION", func(c *Config, v string) error { c.S3Region = v; return nil }},
	{"VLB_S3_BUCKET", func(c *Config, v string) error { c.S3Bucket = v; return nil }},
	{"VLB_S3_PREFIX", func(c *Config, v string) error { c.S3Prefix = v; return nil }},
	{"VLB_S3_PATH_STYLE", setBool(func(c *Config, b bool) { c.S3PathStyle = b })},
	{"VLB_S3_ACCESS_KEY", func(c *Config, v string) error { c.S3AccessKey = v; return nil }},
	{"VLB_S3_SECRET_KEY", func(c *Config, v string) error { c.S3SecretKey = v; return nil }},
	{"VLB_S3_MULTIPART_THRESHOLD", setInt64(func(c *Config, n int64) { c.S3MultipartThreshold = n })},
	{"VLB_S3_MULTIPART_PART_SIZE", setInt64(func(c *Config, n int64) { c.S3MultipartPartSize = n })},
	{"VLB_CONCURRENCY", setInt(func(c *Config, n int) { c.Concurrency = n })},
	{"VLB_IO_TIMEOUT", setDuration(func(c *Config, d time.Duration) { c.IOTimeout = d })},
	{"VLB_JOB_TIMEOUT", setDuration(func(c *Config, d time.Duration) { c.JobTimeout = d })},
	{"VLB_DISK_CHECK_FACTOR", setFloat(func(c *Config, f float64) { c.DiskCheckFactor = f })},
	{"VLB_ORPHAN_SNAPSHOT_AGE", setDuration(func(c *Config, d time.Duration) { c.OrphanSnapshotAge = d })},
}

func applyEnv(cfg *Config) error {
	for _, e := range envEntries {
		raw, ok := os.LookupEnv(e.key)
		if !ok || raw == "" {
			continue
		}
		if err := e.set(cfg, raw); err != nil {
			return errs.Usagef(err, "env %s=%s", e.key, raw)
		}
	}
	return nil
}

func setBool(assign func(*Config, bool)) func(*Config, string) error {
	return func(c *Config, raw string) error {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("parse bool: %w", err)
		}
		assign(c, b)
		return nil
	}
}

func setInt(assign func(*Config, int)) func(*Config, string) error {
	return func(c *Config, raw string) error {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("parse int: %w", err)
		}
		assign(c, n)
		return nil
	}
}

func setDuration(assign func(*Config, time.Duration)) func(*Config, string) error {
	return func(c *Config, raw string) error {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("parse duration: %w", err)
		}
		assign(c, d)
		return nil
	}
}

func setInt64(assign func(*Config, int64)) func(*Config, string) error {
	return func(c *Config, raw string) error {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("parse int64: %w", err)
		}
		assign(c, n)
		return nil
	}
}

func setFloat(assign func(*Config, float64)) func(*Config, string) error {
	return func(c *Config, raw string) error {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("parse float: %w", err)
		}
		assign(c, f)
		return nil
	}
}

// Validate 校验通用约束（格式与取值范围），必填项由各子命令通过 Require 检查。
func (c *Config) Validate() error {
	if c.Concurrency < 1 {
		return errs.Usage("concurrency must be >= 1, got %d", c.Concurrency)
	}
	if c.IOTimeout <= 0 {
		return errs.Usage("io_timeout must be positive, got %s", c.IOTimeout)
	}
	if c.JobTimeout < 0 {
		return errs.Usage("job_timeout must not be negative, got %s", c.JobTimeout)
	}
	switch c.VLAuthMode {
	case AuthModeNone:
	case AuthModeBasic:
		if c.VLUsername == "" || c.VLPassword == "" {
			return errs.Usage("vl_auth_mode basic requires vl_username and vl_password")
		}
	case AuthModeBearer:
		if c.VLToken == "" {
			return errs.Usage("vl_auth_mode bearer requires vl_token")
		}
	default:
		return errs.Usage("vl_auth_mode must be one of none/basic/bearer, got %q", c.VLAuthMode)
	}
	if c.VLRetentionDays < 1 {
		return errs.Usage("vl_retention_days must be >= 1, got %d", c.VLRetentionDays)
	}
	if c.S3MultipartThreshold < 0 {
		return errs.Usage("s3_multipart_threshold must not be negative, got %d", c.S3MultipartThreshold)
	}
	if c.S3MultipartPartSize <= 0 {
		return errs.Usage("s3_multipart_part_size must be positive, got %d", c.S3MultipartPartSize)
	}
	if c.DiskCheckFactor <= 0 {
		return errs.Usage("disk_check_factor must be positive, got %g", c.DiskCheckFactor)
	}
	if c.OrphanSnapshotAge <= 0 {
		return errs.Usage("orphan_snapshot_age must be positive, got %s", c.OrphanSnapshotAge)
	}
	if c.Cluster == "" || c.Node == "" {
		return errs.Usage("cluster and node must not be empty")
	}
	for name, u := range map[string]string{"vl_url": c.VLURL, "s3_endpoint": c.S3Endpoint} {
		if u == "" {
			continue // 必填性由 Require 按命令判断
		}
		if scheme := schemeOf(u); scheme != "http" && scheme != "https" {
			return errs.Usage("%s %q must use http or https", name, u)
		}
	}
	return nil
}

func schemeOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme
}

// 必填字段名，供 Require 使用。
const (
	FieldValuePath  = "vl_url"
	FieldDataPath   = "data_path"
	FieldS3Endpoint = "s3_endpoint"
	FieldS3Bucket   = "s3_bucket"
)

// Require 检查指定字段非空，缺失时返回退出码 2 的错误。
func (c *Config) Require(fields ...string) error {
	getters := map[string]func() string{
		FieldValuePath:  func() string { return c.VLURL },
		FieldDataPath:   func() string { return c.DataPath },
		FieldS3Endpoint: func() string { return c.S3Endpoint },
		FieldS3Bucket:   func() string { return c.S3Bucket },
	}
	for _, f := range fields {
		get, ok := getters[f]
		if !ok {
			return errs.Usage("unknown required field %q", f)
		}
		if get() == "" {
			return errs.Usage("missing required config %s (set via flag, VLB_* env or config file)", f)
		}
	}
	return nil
}
