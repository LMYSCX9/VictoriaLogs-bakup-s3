package report

import (
	"bytes"
	"strings"
	"testing"
)

func TestJSONSingleLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := Summary{
		Op:        "backup",
		Status:    "success",
		Cluster:   "prod",
		Node:      "vlstorage-0",
		Partition: "2026-10-27",
		Files:     3,
		Bytes:     1024,
		ElapsedMS: 42,
	}
	if err := JSON(&buf, s); err != nil {
		t.Fatalf("JSON() error = %v", err)
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Error("output should end with newline")
	}
	out = strings.TrimSuffix(out, "\n")
	if strings.Contains(out, "\n") {
		t.Errorf("output must be a single line, got %q", out)
	}
	for _, want := range []string{
		`"op":"backup"`, `"status":"success"`, `"cluster":"prod"`,
		`"node":"vlstorage-0"`, `"partition":"2026-10-27"`,
		`"files":3`, `"bytes":1024`, `"elapsed_ms":42`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q should contain %q", out, want)
		}
	}
}

func TestJSONOmitEmpty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := Summary{Op: "version", Status: "success", ElapsedMS: 1}
	if err := JSON(&buf, s); err != nil {
		t.Fatalf("JSON() error = %v", err)
	}

	out := strings.TrimSuffix(buf.String(), "\n")
	for _, unwanted := range []string{"cluster", "node", "partition", "files", "bytes", "error_code"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output %q should not contain empty field %q", out, unwanted)
		}
	}
}

func TestTable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := Summary{
		Op:        "delete",
		Status:    "success",
		Partition: "2026-10-27",
		Files:     12438,
		Bytes:     62586441728,
		ElapsedMS: 3001,
		ErrorCode: 6,
	}
	if err := Table(&buf, s); err != nil {
		t.Fatalf("Table() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"op", "delete", "status", "success", "partition", "2026-10-27",
		"files", "12438", "bytes", "62586441728", "elapsed_ms", "3001", "error_code", "6",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output %q should contain %q", out, want)
		}
	}
}

// R-OPS-02：op 专属扩展字段仅在该 op 的摘要中出现（omitempty）。
func TestJSONOpSpecificFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := Summary{
		Op:               "restore",
		Status:           "success",
		Cluster:          "prod",
		Node:             "vlstorage-0",
		Partition:        "2026-10-27",
		Files:            12,
		Bytes:            4096,
		ElapsedMS:        88,
		SmokeQuery:       "ok",
		ReplacedPath:     "/data/partitions/2026-10-27.replaced-1",
		CrossNode:        true,
		SnapshotRetained: true,
		Today:            true,
		Skipped:          3,
	}
	if err := JSON(&buf, s); err != nil {
		t.Fatalf("JSON() error = %v", err)
	}

	out := strings.TrimSuffix(buf.String(), "\n")
	for _, want := range []string{
		`"smoke_query":"ok"`, `"replaced_path":"/data/partitions/2026-10-27.replaced-1"`,
		`"cross_node":true`, `"snapshot_retained":true`, `"today":true`, `"skipped":3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q should contain %q", out, want)
		}
	}
	// 其他 op 的专属字段不应出现。
	for _, unwanted := range []string{"has_backup", "dry_run", `"units"`, `"objects"`, "leftovers", `"path":"`} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output %q should not contain field %q", out, unwanted)
		}
	}
}

// §11.3：gc 摘要扩展字段。
func TestJSONGCSummary(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := Summary{
		Op:        "gc",
		Status:    "success",
		ElapsedMS: 120,
		DryRun:    true,
		Units:     2,
		Objects:   30,
		Leftovers: 1,
		Bytes:     999,
	}
	if err := JSON(&buf, s); err != nil {
		t.Fatalf("JSON() error = %v", err)
	}

	out := strings.TrimSuffix(buf.String(), "\n")
	for _, want := range []string{
		`"dry_run":true`, `"units":2`, `"objects":30`, `"leftovers":1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q should contain %q", out, want)
		}
	}
}

// 多分区批量作业：partitions 数组扩展字段。
func TestJSONMultiPartition(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := Summary{
		Op:         "backup",
		Status:     "success",
		ElapsedMS:  10,
		Partitions: []string{"2026-10-26", "2026-10-27"},
	}
	if err := JSON(&buf, s); err != nil {
		t.Fatalf("JSON() error = %v", err)
	}

	out := strings.TrimSuffix(buf.String(), "\n")
	if !strings.Contains(out, `"partitions":["2026-10-26","2026-10-27"]`) {
		t.Errorf("output %q should contain partitions array", out)
	}
}
