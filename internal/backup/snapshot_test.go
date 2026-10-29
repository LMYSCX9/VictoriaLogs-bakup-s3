package backup

import "testing"

// TestSnapshotPartition 覆盖真实 VL 快照路径形态（<…>/partitions/<date>/snapshots/<name>）
// 与各类不识别形态：非 snapshots 布局、非法日期、过短路径。
func TestSnapshotPartition(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"posix real layout", "/vmlog-data/partitions/20261027/snapshots/16A9C298143BD0A8-16A9C298143BD0A8", "20261027"},
		{"windows real layout", `C:\vmlog-data\partitions\20261027\snapshots\16A9C298143BD0A8`, "20261027"},
		{"trailing separator", "/vmlog-data/partitions/20261027/snapshots/16A9C298/", "20261027"},
		{"relative minimal", "20261027/snapshots/x", "20261027"},
		{"not snapshots layout", "/vmlog-data/partitions/20261027/part.0", ""},
		{"date as basename only", "/vmlog-data/partitions/20261027", ""},
		{"invalid date", "/vmlog-data/partitions/20261327/snapshots/x", ""},
		{"short date", "/vmlog-data/partitions/2026102/snapshots/x", ""},
		{"too short", "snapshots/20261027", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapshotPartition(tc.path); got != tc.want {
				t.Errorf("snapshotPartition(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestScanSnapshotDirsRelToSnapshotRoot 校验 manifest 相对路径以快照根为基准
// （indexdb/…、datadb/…），恢复时据此整体还原 partitions/<date>/ 结构。
func TestScanSnapshotDirsRelToSnapshotRoot(t *testing.T) {
	part := "20261027"
	dir := makeSnapshotDir(t, t.TempDir(), part, "16A9C298143BD0A8", map[string]string{
		"indexdb/metaindex.bin":                                   "idx",
		"datadb/smallparts/0_1_19700101_20260102_20261027_1.part": "data",
	})

	got, err := scanSnapshotDirs([]string{dir})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	want := []string{
		"datadb/smallparts/0_1_19700101_20260102_20261027_1.part",
		"indexdb/metaindex.bin",
	}
	for i, e := range got {
		if e.Path != want[i] {
			t.Errorf("entry[%d].path = %q, want %q", i, e.Path, want[i])
		}
	}
}
