package setup

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// writeFiles 落盘 config.toml（0644）与 credentials（0600）：
// 目标目录不存在则创建（0700）；credentials 无任何敏感项时不创建该文件
// （R-SU-01：留空的项不写入）。必须在连通性验证通过后调用（R-SU-04）。
func writeFiles(dir string, a *Answers) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errs.Usagef(err, "create output dir %s", dir)
	}
	if err := writeTOML(filepath.Join(dir, "config.toml"), 0o644, configMap(a)); err != nil {
		return err
	}
	cred := credentialsMap(a)
	if len(cred) == 0 {
		return nil
	}
	return writeTOML(filepath.Join(dir, "credentials"), 0o600, cred)
}

func writeTOML(path string, perm os.FileMode, v any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return errs.Usagef(err, "create %s", path)
	}
	if err := toml.NewEncoder(f).Encode(v); err != nil {
		f.Close()
		return errs.Untyped(err, "encode %s", path)
	}
	if err := f.Close(); err != nil {
		return errs.Untyped(err, "close %s", path)
	}
	return nil
}

// configMap 生成 config.toml 内容（不含任何敏感项，C9）。
// 未询问到的作业参数由 config.Load 的默认值兜底，不写入文件。
func configMap(a *Answers) map[string]any {
	m := map[string]any{
		"vl_url":            a.VLURL,
		"vl_auth_mode":      a.AuthMode,
		"vl_retention_days": config.Default().VLRetentionDays,
		"data_path":         a.DataPath,
		"cluster":           a.Cluster,
		"node":              a.Node,
		"s3_endpoint":       a.S3Endpoint,
		"s3_bucket":         a.S3Bucket,
		"s3_prefix":         a.S3Prefix,
		"s3_path_style":     a.S3PathStyle,
		"concurrency":       config.Default().Concurrency,
	}
	if a.S3Region != "" {
		m["s3_region"] = a.S3Region
	}
	return m
}

// credentialsMap 只收集非空敏感项；空值项不写入（R-SU-01）。
func credentialsMap(a *Answers) map[string]string {
	m := map[string]string{}
	if a.S3AccessKey != "" {
		m["s3_access_key"] = a.S3AccessKey
	}
	if a.S3SecretKey != "" {
		m["s3_secret_key"] = a.S3SecretKey
	}
	if a.VLUsername != "" {
		m["vl_username"] = a.VLUsername
	}
	if a.VLPassword != "" {
		m["vl_password"] = a.VLPassword
	}
	if a.VLToken != "" {
		m["vl_token"] = a.VLToken
	}
	return m
}

// printSummary 输出最终摘要；敏感值一律 ***（R-SU-03）。
func (w *Wizard) printSummary(a *Answers) {
	out := w.opts.Stdout
	cfgPath := filepath.Join(w.opts.OutputDir, "config.toml")
	fmt.Fprintln(out, "配置已生成:")
	fmt.Fprintf(out, "  config        : %s (0644)\n", cfgPath)
	if len(credentialsMap(a)) > 0 {
		fmt.Fprintf(out, "  credentials   : %s (0600)\n", filepath.Join(w.opts.OutputDir, "credentials"))
	}
	fmt.Fprintf(out, "  vl_url        : %s\n", a.VLURL)
	fmt.Fprintf(out, "  auth_mode     : %s\n", a.AuthMode)
	switch a.AuthMode {
	case config.AuthModeBasic:
		fmt.Fprintf(out, "  vl_username   : %s\n", a.VLUsername)
		fmt.Fprintf(out, "  vl_password   : ***\n")
	case config.AuthModeBearer:
		fmt.Fprintf(out, "  vl_token      : ***\n")
	}
	fmt.Fprintf(out, "  data_path     : %s\n", a.DataPath)
	fmt.Fprintf(out, "  cluster/node  : %s / %s\n", a.Cluster, a.Node)
	fmt.Fprintf(out, "  s3_endpoint   : %s\n", a.S3Endpoint)
	if a.S3Region != "" {
		fmt.Fprintf(out, "  s3_region     : %s\n", a.S3Region)
	}
	fmt.Fprintf(out, "  s3_bucket     : %s\n", a.S3Bucket)
	fmt.Fprintf(out, "  s3_prefix     : %s\n", a.S3Prefix)
	fmt.Fprintf(out, "  s3_path_style : %t\n", a.S3PathStyle)
	if a.S3AccessKey != "" {
		fmt.Fprintf(out, "  s3_access_key : ***\n")
	}
	if a.S3SecretKey != "" {
		fmt.Fprintf(out, "  s3_secret_key : ***\n")
	}
	fmt.Fprintf(out, "下一步示例: vlogs-backup --config %s backup --date 2026-10-27 --dry-run\n", cfgPath)
}
