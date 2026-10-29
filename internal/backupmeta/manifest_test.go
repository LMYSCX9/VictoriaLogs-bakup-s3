package backupmeta

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// validManifest 返回一个通过校验的基准 manifest。
func validManifest() Manifest {
	return Manifest{
		FormatVersion: FormatVersion,
		Cluster:       "prod-logs",
		Node:          "vlstorage-0",
		Partition:     "20261027",
		CreatedAt:     time.Date(2026, 10, 28, 1, 12, 3, 0, time.UTC),
		SnapshotAt:    time.Date(2026, 10, 28, 1, 11, 58, 0, time.UTC),
		VLVersion:     "v1.25.0",
		ToolVersion:   "0.1.0",
		FileCount:     2,
		TotalBytes:    3145728,
		Entries: []Entry{
			{Path: "indexdb/a.bin", Size: 1048576, SHA256: strings.Repeat("a", 64)},
			{Path: "parts/b/c.bin", Size: 2097152, SHA256: strings.Repeat("b", 64)},
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Manifest)
		wantErr  string // 空串表示期望通过
		isTooNew bool
	}{
		{name: "valid", mutate: func(*Manifest) {}},
		{name: "valid_zero_entries", mutate: func(m *Manifest) {
			m.Entries = nil
			m.FileCount = 0
			m.TotalBytes = 0
		}},
		{name: "version_missing", mutate: func(m *Manifest) { m.FormatVersion = 0 }, wantErr: "format_version"},
		{name: "version_too_new", mutate: func(m *Manifest) { m.FormatVersion = 99 }, wantErr: "please upgrade the tool", isTooNew: true},
		{name: "version_negative", mutate: func(m *Manifest) { m.FormatVersion = -1 }, wantErr: "format_version"},
		{name: "cluster_missing", mutate: func(m *Manifest) { m.Cluster = "" }, wantErr: "cluster is missing"},
		{name: "node_missing", mutate: func(m *Manifest) { m.Node = "" }, wantErr: "node is missing"},
		{name: "partition_short", mutate: func(m *Manifest) { m.Partition = "2026102" }, wantErr: "partition"},
		{name: "partition_non_digit", mutate: func(m *Manifest) { m.Partition = "2026102a" }, wantErr: "partition"},
		{name: "partition_bad_month", mutate: func(m *Manifest) { m.Partition = "20261301" }, wantErr: "partition"},
		{name: "partition_bad_day", mutate: func(m *Manifest) { m.Partition = "20260231" }, wantErr: "partition"},
		{name: "created_at_missing", mutate: func(m *Manifest) { m.CreatedAt = time.Time{} }, wantErr: "created_at is missing"},
		{name: "snapshot_at_missing", mutate: func(m *Manifest) { m.SnapshotAt = time.Time{} }, wantErr: "snapshot_at is missing"},
		{name: "vl_version_missing", mutate: func(m *Manifest) { m.VLVersion = "" }, wantErr: "vl_version is missing"},
		{name: "tool_version_missing", mutate: func(m *Manifest) { m.ToolVersion = "" }, wantErr: "tool_version is missing"},
		{name: "file_count_mismatch", mutate: func(m *Manifest) { m.FileCount = 3 }, wantErr: "file_count 3 does not match 2 entries"},
		{name: "total_bytes_mismatch", mutate: func(m *Manifest) { m.TotalBytes++ }, wantErr: "total_bytes"},
		{name: "entry_path_empty", mutate: func(m *Manifest) { m.Entries[0].Path = "" }, wantErr: "not a safe relative path"},
		{name: "entry_path_absolute", mutate: func(m *Manifest) { m.Entries[0].Path = "/etc/a.bin" }, wantErr: "not a safe relative path"},
		{name: "entry_path_traversal", mutate: func(m *Manifest) { m.Entries[0].Path = "../a.bin" }, wantErr: "not a safe relative path"},
		{name: "entry_path_dot", mutate: func(m *Manifest) { m.Entries[0].Path = "./a.bin" }, wantErr: "not a safe relative path"},
		{name: "entry_path_backslash", mutate: func(m *Manifest) { m.Entries[0].Path = `parts\a.bin` }, wantErr: "not a safe relative path"},
		{name: "entry_path_drive_colon", mutate: func(m *Manifest) { m.Entries[0].Path = "C:/a.bin" }, wantErr: "not a safe relative path"},
		{name: "entry_path_empty_segment", mutate: func(m *Manifest) { m.Entries[0].Path = "a//b.bin" }, wantErr: "not a safe relative path"},
		{name: "entry_size_negative", mutate: func(m *Manifest) { m.Entries[0].Size = -1; m.TotalBytes-- }, wantErr: "negative size"},
		{name: "entry_sha256_short", mutate: func(m *Manifest) { m.Entries[0].SHA256 = "abcd" }, wantErr: "invalid sha256"},
		{name: "entry_sha256_non_hex", mutate: func(m *Manifest) { m.Entries[0].SHA256 = strings.Repeat("z", 64) }, wantErr: "invalid sha256"},
		{name: "entry_duplicate", mutate: func(m *Manifest) { m.Entries[1].Path = m.Entries[0].Path }, wantErr: "duplicate entry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validManifest()
			tt.mutate(&m)
			err := m.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want containing %q", err.Error(), tt.wantErr)
			}
			if tt.isTooNew && !errors.Is(err, ErrVersionTooNew) {
				t.Fatalf("Validate() = %q, want errors.Is ErrVersionTooNew", err.Error())
			}
		})
	}
}

func TestCheckVersion(t *testing.T) {
	if err := CheckVersion(FormatVersion); err != nil {
		t.Fatalf("CheckVersion(%d) = %v, want nil", FormatVersion, err)
	}
	err := CheckVersion(FormatVersion + 1)
	if !errors.Is(err, ErrVersionTooNew) {
		t.Fatalf("CheckVersion(%d) = %v, want ErrVersionTooNew", FormatVersion+1, err)
	}
	if err := CheckVersion(0); err == nil || errors.Is(err, ErrVersionTooNew) {
		t.Fatalf("CheckVersion(0) = %v, want plain error", err)
	}
}

func TestParse(t *testing.T) {
	t.Run("roundtrip valid", func(t *testing.T) {
		want := validManifest()
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got, err := Parse(data)
		if err != nil {
			t.Fatalf("Parse() = %v, want nil", err)
		}
		if got.Partition != want.Partition || got.Cluster != want.Cluster ||
			got.FileCount != want.FileCount || got.TotalBytes != want.TotalBytes ||
			len(got.Entries) != len(want.Entries) {
			t.Fatalf("Parse() roundtrip mismatch: %+v", got)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		if _, err := Parse([]byte("{not json")); err == nil {
			t.Fatal("Parse() = nil, want parse error")
		}
	})

	t.Run("valid json but invalid manifest", func(t *testing.T) {
		data, err := json.Marshal(map[string]any{"format_version": 2})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_, err = Parse(data)
		if !errors.Is(err, ErrVersionTooNew) {
			t.Fatalf("Parse() = %v, want ErrVersionTooNew", err)
		}
	})
}
