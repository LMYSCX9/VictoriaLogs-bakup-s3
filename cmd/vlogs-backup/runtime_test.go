package main

import (
	"testing"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
)

// 回归测试（R-ST-01 布局裁决）：local 模式下 KeyBuilder 前缀必须为空，
// backup/restore/purge 的 Job Options 传参也必须经过 effectiveKeyPrefix，
// 禁止直接读 cfg.S3Prefix（E2E 曾因三处直读导致 local 布局多出 prefix 层）。
func TestEffectiveKeyPrefix(t *testing.T) {
	cases := []struct {
		name        string
		storageType string
		localPath   string
		s3Prefix    string
		want        string
	}{
		{"s3 默认", config.StorageTypeS3, "", "vlogs", "vlogs"},
		{"s3 空前缀", config.StorageTypeS3, "", "", ""},
		{"local 按 storage_type", config.StorageTypeLocal, "/tmp/x", "vlogs", ""},
		{"local_backup_path 优先级高于 storage_type", config.StorageTypeS3, "/tmp/x", "vlogs", ""},
		{"storage_type 空值回退 s3", "", "", "vlogs", "vlogs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				StorageType:     tc.storageType,
				LocalBackupPath: tc.localPath,
				S3Prefix:        tc.s3Prefix,
			}
			if got := effectiveKeyPrefix(cfg); got != tc.want {
				t.Fatalf("effectiveKeyPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}
