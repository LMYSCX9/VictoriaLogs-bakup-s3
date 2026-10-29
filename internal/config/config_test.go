package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load(\"\") error = %v", err)
	}
	if cfg.Cluster != "default" || cfg.Node != "default" {
		t.Errorf("cluster/node default = %q/%q, want default/default", cfg.Cluster, cfg.Node)
	}
	if cfg.VLAuthMode != AuthModeNone {
		t.Errorf("vl_auth_mode default = %q, want %q", cfg.VLAuthMode, AuthModeNone)
	}
	if cfg.VLRetentionDays != 181 {
		t.Errorf("vl_retention_days default = %d, want 181", cfg.VLRetentionDays)
	}
	if cfg.S3Prefix != "vlogs" {
		t.Errorf("s3_prefix default = %q, want vlogs", cfg.S3Prefix)
	}
	if !cfg.S3PathStyle {
		t.Error("s3_path_style default should be true")
	}
	if cfg.Concurrency != 8 {
		t.Errorf("concurrency default = %d, want 8", cfg.Concurrency)
	}
	if cfg.IOTimeout != 10*time.Minute {
		t.Errorf("io_timeout default = %s, want 10m", cfg.IOTimeout)
	}
	if cfg.JobTimeout != 0 {
		t.Errorf("job_timeout default = %s, want 0", cfg.JobTimeout)
	}
}

func TestLoadFileWithCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "config.toml", `
vl_url = "http://127.0.0.1:9428"
vl_auth_mode = "basic"
vl_retention_days = 90
data_path = "/var/lib/victoria-logs"
cluster = "prod"
node = "vlstorage-0"
s3_endpoint = "http://rustfs:9000"
s3_bucket = "logs-backup"
s3_prefix = "vlogs"
s3_path_style = true
concurrency = 4
io_timeout = "5m"
job_timeout = "2h"
`)
	writeFile(t, dir, "credentials", `
s3_access_key = "AKID"
s3_secret_key = "SECRET"
vl_username = "VLUSER"
vl_password = "VLPASS"
`)

	cfg, err := Load(context.Background(), filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.VLURL != "http://127.0.0.1:9428" {
		t.Errorf("vl_url = %q", cfg.VLURL)
	}
	if cfg.VLAuthMode != AuthModeBasic {
		t.Errorf("vl_auth_mode = %q, want basic", cfg.VLAuthMode)
	}
	if cfg.VLRetentionDays != 90 {
		t.Errorf("vl_retention_days = %d, want 90", cfg.VLRetentionDays)
	}
	if cfg.DataPath != "/var/lib/victoria-logs" {
		t.Errorf("data_path = %q", cfg.DataPath)
	}
	if cfg.Cluster != "prod" || cfg.Node != "vlstorage-0" {
		t.Errorf("cluster/node = %q/%q", cfg.Cluster, cfg.Node)
	}
	if cfg.S3Bucket != "logs-backup" {
		t.Errorf("s3_bucket = %q", cfg.S3Bucket)
	}
	if cfg.Concurrency != 4 {
		t.Errorf("concurrency = %d, want 4", cfg.Concurrency)
	}
	if cfg.IOTimeout != 5*time.Minute {
		t.Errorf("io_timeout = %s, want 5m", cfg.IOTimeout)
	}
	if cfg.JobTimeout != 2*time.Hour {
		t.Errorf("job_timeout = %s, want 2h", cfg.JobTimeout)
	}
	if cfg.S3AccessKey != "AKID" || cfg.S3SecretKey != "SECRET" ||
		cfg.VLUsername != "VLUSER" || cfg.VLPassword != "VLPASS" {
		t.Error("credentials values not loaded")
	}
}

func TestLoadMissingCredentialsIsOptional(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "config.toml", `vl_url = "http://127.0.0.1:9428"`)

	cfg, err := Load(context.Background(), filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.S3AccessKey != "" {
		t.Error("credentials should be empty when file is absent")
	}
}

func TestLoadBrokenFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeFile(t, dir, "config.toml", `vl_url = [1, 2`)

	_, err := Load(context.Background(), path)
	if errs.ExitCode(err) != errs.ExitUsage {
		t.Errorf("ExitCode = %d, want %d (error = %v)", errs.ExitCode(err), errs.ExitUsage, err)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	// t.Setenv 与 t.Parallel 互斥，此测试保持串行。

	dir := t.TempDir()
	writeFile(t, dir, "config.toml", `
vl_url = "http://from-file:9428"
s3_bucket = "from-file"
concurrency = 2
`)

	t.Setenv("VLB_VL_URL", "http://from-env:9428")
	t.Setenv("VLB_VL_AUTH_MODE", "bearer")
	t.Setenv("VLB_VL_TOKEN", "tok")
	t.Setenv("VLB_CONCURRENCY", "16")
	t.Setenv("VLB_JOB_TIMEOUT", "30m")
	t.Setenv("VLB_S3_PATH_STYLE", "false")

	cfg, err := Load(context.Background(), filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.VLURL != "http://from-env:9428" {
		t.Errorf("env should override file, vl_url = %q", cfg.VLURL)
	}
	if cfg.VLAuthMode != AuthModeBearer || cfg.VLToken != "tok" {
		t.Errorf("vl auth env = %q/%q, want bearer/tok", cfg.VLAuthMode, cfg.VLToken)
	}
	if cfg.Concurrency != 16 {
		t.Errorf("env should override file, concurrency = %d", cfg.Concurrency)
	}
	if cfg.JobTimeout != 30*time.Minute {
		t.Errorf("job_timeout = %s, want 30m", cfg.JobTimeout)
	}
	if cfg.S3PathStyle {
		t.Error("VLB_S3_PATH_STYLE=false should override file default true")
	}
	if cfg.S3Bucket != "from-file" {
		t.Errorf("unset env should not override file, s3_bucket = %q", cfg.S3Bucket)
	}
}

func TestEnvInvalidValue(t *testing.T) {
	// t.Setenv 与 t.Parallel 互斥，此测试保持串行。
	t.Setenv("VLB_CONCURRENCY", "not-a-number")

	_, err := Load(context.Background(), "")
	if errs.ExitCode(err) != errs.ExitUsage {
		t.Errorf("ExitCode = %d, want %d (error = %v)", errs.ExitCode(err), errs.ExitUsage, err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid minimal", func(c *Config) {}, false},
		{"concurrency zero", func(c *Config) { c.Concurrency = 0 }, true},
		{"concurrency negative", func(c *Config) { c.Concurrency = -1 }, true},
		{"io_timeout zero", func(c *Config) { c.IOTimeout = 0 }, true},
		{"job_timeout negative", func(c *Config) { c.JobTimeout = -time.Second }, true},
		{"empty cluster", func(c *Config) { c.Cluster = "" }, true},
		{"bad vl_url scheme", func(c *Config) { c.VLURL = "ftp://vl:9428" }, true},
		{"good vl_url", func(c *Config) { c.VLURL = "https://vl:9428" }, false},
		{"bad s3 scheme", func(c *Config) { c.S3Endpoint = "s3.amazonaws.com" }, true},
		{"empty vl_auth_mode", func(c *Config) { c.VLAuthMode = "" }, true},
		{"bad vl_auth_mode", func(c *Config) { c.VLAuthMode = "apikey" }, true},
		{"basic missing username", func(c *Config) {
			c.VLAuthMode = AuthModeBasic
			c.VLPassword = "p"
		}, true},
		{"basic ok", func(c *Config) {
			c.VLAuthMode = AuthModeBasic
			c.VLUsername = "u"
			c.VLPassword = "p"
		}, false},
		{"bearer missing token", func(c *Config) { c.VLAuthMode = AuthModeBearer }, true},
		{"bearer ok", func(c *Config) {
			c.VLAuthMode = AuthModeBearer
			c.VLToken = "t"
		}, false},
		{"retention zero", func(c *Config) { c.VLRetentionDays = 0 }, true},
		{"retention negative", func(c *Config) { c.VLRetentionDays = -1 }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tt.mutate(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && errs.ExitCode(err) != errs.ExitUsage {
				t.Errorf("ExitCode = %d, want %d", errs.ExitCode(err), errs.ExitUsage)
			}
		})
	}
}

func TestRequire(t *testing.T) {
	t.Parallel()

	t.Run("missing field", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		// Require 按传入顺序返回第一个缺失字段，Default() 中 vl_url 先缺。
		err := cfg.Require(FieldValuePath, FieldDataPath)
		if errs.ExitCode(err) != errs.ExitUsage {
			t.Errorf("ExitCode = %d, want %d (error = %v)", errs.ExitCode(err), errs.ExitUsage, err)
		}
		if err == nil || !strings.Contains(err.Error(), FieldValuePath) {
			t.Errorf("error should name the missing field, got %v", err)
		}
	})

	t.Run("all present", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.VLURL = "http://vl:9428"
		cfg.DataPath = "/var/lib/victoria-logs"
		if err := cfg.Require(FieldValuePath, FieldDataPath); err != nil {
			t.Errorf("Require() unexpected error = %v", err)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		if err := cfg.Require("nope"); errs.ExitCode(err) != errs.ExitUsage {
			t.Errorf("ExitCode = %d, want %d", errs.ExitCode(err), errs.ExitUsage)
		}
	})
}
