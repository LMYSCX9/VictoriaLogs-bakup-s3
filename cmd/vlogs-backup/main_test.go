package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// runCLI 以非交互 stdin 调用 run，简化纯参数路径测试。
func runCLI(args ...string) (code int, stdout, stderr string, stdin *strings.Reader) {
	var out, errb bytes.Buffer
	in := strings.NewReader("")
	code = run(args, in, &out, &errb)
	return code, out.String(), errb.String(), in
}

func TestRunVersionText(t *testing.T) {
	code, stdout, stderr, _ := runCLI("version")

	if code != errs.ExitOK {
		t.Fatalf("exit code = %d, want %d", code, errs.ExitOK)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	info := currentVersion()
	for _, want := range []string{info.Version, info.Commit, info.BuildTime, runtime.Version()} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output %q does not contain %q", stdout, want)
		}
	}
}

func TestRunVersionJSON(t *testing.T) {
	code, stdout, stderr, _ := runCLI("version", "--json")

	if code != errs.ExitOK {
		t.Fatalf("exit code = %d, want %d", code, errs.ExitOK)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if strings.Contains(strings.TrimSuffix(stdout, "\n"), "\n") {
		t.Errorf("output %q must be single line", stdout)
	}

	var got map[string]string
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}
	info := currentVersion()
	if got["version"] != info.Version {
		t.Errorf("version = %q, want %q", got["version"], info.Version)
	}
	if got["commit"] != info.Commit {
		t.Errorf("commit = %q, want %q", got["commit"], info.Commit)
	}
	if got["build_time"] != info.BuildTime {
		t.Errorf("build_time = %q, want %q", got["build_time"], info.BuildTime)
	}
	if got["go_version"] == "" {
		t.Error("go_version is empty")
	}
}

func TestRunVersionRejectsExtraArgs(t *testing.T) {
	t.Run("unknown flag", func(t *testing.T) {
		code, _, _, _ := runCLI("version", "--no-such-flag")
		if code != errs.ExitUsage {
			t.Fatalf("exit code = %d, want %d", code, errs.ExitUsage)
		}
	})
	t.Run("positional arg", func(t *testing.T) {
		code, _, _, _ := runCLI("version", "extra")
		if code != errs.ExitUsage {
			t.Fatalf("exit code = %d, want %d", code, errs.ExitUsage)
		}
	})
}

func TestRunNoArgs(t *testing.T) {
	code, _, stderr, _ := runCLI()

	if code != errs.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(stderr, "用法: vlogs-backup") {
		t.Errorf("stderr %q should print usage", stderr)
	}
}

func TestRunHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			code, stdout, _, _ := runCLI(arg)
			if code != errs.ExitOK {
				t.Fatalf("exit code = %d, want %d", code, errs.ExitOK)
			}
			if !strings.Contains(stdout, "setup") {
				t.Errorf("usage %q should list setup command", stdout)
			}
		})
	}
}

func TestRunUnknownCommand(t *testing.T) {
	code, _, stderr, _ := runCLI("no-such-command")

	if code != errs.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(stderr, "未知命令") {
		t.Errorf("stderr %q should mention unknown command", stderr)
	}
}

func TestPrescanConfig(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantPath     string
		wantExplicit bool
		wantRest     int
	}{
		{"none", []string{"--date", "2026-10-27"}, "", false, 2},
		{"double dash", []string{"--config", "/tmp/c.toml", "--json"}, "/tmp/c.toml", true, 1},
		{"single dash", []string{"-config", "/tmp/c.toml"}, "/tmp/c.toml", true, 0},
		{"equals", []string{"--config=/tmp/c.toml", "--json"}, "/tmp/c.toml", true, 1},
		{"dangling", []string{"--config"}, "", true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, explicit, rest := prescanConfig(tc.args)
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			if explicit != tc.wantExplicit {
				t.Errorf("explicit = %v, want %v", explicit, tc.wantExplicit)
			}
			if len(rest) != tc.wantRest {
				t.Errorf("rest = %v, want %d items", rest, tc.wantRest)
			}
		})
	}
}

func TestExplicitConfigMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.toml")
	code, _, stderr, _ := runCLI("list", "--config", missing)
	if code != errs.ExitUsage {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, errs.ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "parse config file") {
		t.Errorf("stderr %q should mention config parse failure", stderr)
	}
}

// TestRunGlobalConfigFlag 覆盖全局 --config 形式（spec R-SU-07）：
// vlogs-backup --config <path> <command> ...，--config 位于子命令之前。
func TestRunGlobalConfigFlag(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.toml")
	t.Run("space form", func(t *testing.T) {
		code, _, stderr, _ := runCLI("--config", missing, "list")
		if code != errs.ExitUsage {
			t.Fatalf("exit code = %d, want %d (stderr=%q)", code, errs.ExitUsage, stderr)
		}
		if !strings.Contains(stderr, "parse config file") {
			t.Errorf("stderr %q should mention config parse failure", stderr)
		}
	})
	t.Run("equals form", func(t *testing.T) {
		code, _, stderr, _ := runCLI("--config="+missing, "list")
		if code != errs.ExitUsage {
			t.Fatalf("exit code = %d, want %d (stderr=%q)", code, errs.ExitUsage, stderr)
		}
		if !strings.Contains(stderr, "parse config file") {
			t.Errorf("stderr %q should mention config parse failure", stderr)
		}
	})
}

func TestRunGlobalConfigWithoutCommand(t *testing.T) {
	code, _, stderr, _ := runCLI("--config", filepath.Join(t.TempDir(), "c.toml"))
	if code != errs.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(stderr, "用法: vlogs-backup") {
		t.Errorf("stderr %q should print usage", stderr)
	}
}

// TestRunSubcommandConfigOverridesGlobal：子命令形式优先于全局形式（后值覆盖前值）。
func TestRunSubcommandConfigOverridesGlobal(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.toml") // 全局形式指向不存在的文件
	partial := filepath.Join(dir, "partial.toml")
	if err := os.WriteFile(partial, []byte("cluster = \"c\"\nnode = \"n\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// 若全局形式胜出，报错为 "parse config file"（文件缺失）；
	// 子命令形式胜出时配置可解析但缺必填项，报错为 "missing required config"。
	code, _, stderr, _ := runCLI("--config", missing, "gc", "--config", partial)
	if code != errs.ExitUsage {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, errs.ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "missing required config") {
		t.Errorf("stderr %q should come from the subcommand config", stderr)
	}
}

func TestBackupFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"bad if-exists", []string{"backup", "--if-exists", "overwrite"}},
		{"date with latest", []string{"backup", "--date", "2026-10-27", "--latest", "3"}},
		{"date with range", []string{"backup", "--date", "2026-10-27", "--from", "2026-10-20", "--to", "2026-10-21"}},
		{"bad date", []string{"backup", "--date", "2026-13-01"}},
		{"unknown flag", []string{"backup", "--no-such-flag"}},
		{"positional arg", []string{"backup", "extra"}},
		{"latest with range", []string{"backup", "--latest", "3", "--from", "2026-10-20", "--to", "2026-10-21"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _, _ := runCLI(tc.args...)
			if code != errs.ExitUsage {
				t.Fatalf("exit code = %d, want %d", code, errs.ExitUsage)
			}
		})
	}
}

func TestRestoreFlagValidation(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{"bad on-existing", []string{"restore", "--on-existing", "replace"}, "--on-existing"},
		{"non-tty without yes", []string{"restore", "--date", yesterday}, "--yes"},
		{"today without force-today", []string{"restore", "--date", today, "--yes"}, "--force-today"},
		{"bad date", []string{"restore", "--date", "27-10-2026"}, "--date"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr, _ := runCLI(tc.args...)
			if code != errs.ExitUsage {
				t.Fatalf("exit code = %d, want %d (stderr=%q)", code, errs.ExitUsage, stderr)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr %q should contain %q", stderr, tc.wantStderr)
			}
		})
	}
}

func TestDeleteFlagValidation(t *testing.T) {
	code, _, stderr, _ := runCLI("delete", "--date", "2026-10-27")
	if code != errs.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr %q should require --yes in non-interactive environment", stderr)
	}
}

func TestListFlagValidation(t *testing.T) {
	code, _, stderr, _ := runCLI("list", "--from", "2026-10-01")
	if code != errs.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(stderr, "--from and --to") {
		t.Errorf("stderr %q should mention --from/--to pairing", stderr)
	}
}

func TestMissingRequiredConfig(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(empty, []byte("cluster = \"c\"\nnode = \"n\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	code, _, stderr, _ := runCLI("gc", "--config", empty)
	if code != errs.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(stderr, "missing required config") {
		t.Errorf("stderr %q should mention missing required config", stderr)
	}
}

func TestConfirmPartitionRequiresFullDate(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"yes letter", "y\n", true},
		{"empty line", "\n", true},
		{"wrong date", "2026-10-26\n", true},
		{"eof", "", true},
		{"correct date", "2026-10-27\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			e := &env{stdin: strings.NewReader(tc.input), stdout: &bytes.Buffer{}, stderr: &stderr}
			err := confirmPartition(e, "20261027")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if code := errs.ExitCode(err); code != errs.ExitCanceled {
					t.Errorf("exit code = %d, want %d", code, errs.ExitCanceled)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(stderr.String(), "2026-10-27") {
				t.Errorf("prompt %q should contain the full date", stderr.String())
			}
		})
	}
}

func TestParseDay(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"2026-10-27", "20261027", false},
		{"20261027", "20261027", false},
		{"2026-02-30", "", true},
		{"", "", true},
		{"27/10/2026", "", true},
	}
	for _, tc := range tests {
		got, err := parseDay("date", tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseDay(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDay(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDay(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBackupDryRunWithVLStub 覆盖 R-OPS-04 的探测路径与 --json 单行摘要契约。
func TestBackupDryRunWithVLStub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/partition/snapshot/list", "/internal/partition/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfgBody := "vl_url = \"" + srv.URL + "\"\n" +
		"data_path = \"" + filepath.ToSlash(dir) + "\"\n" +
		"s3_endpoint = \"http://127.0.0.1:9000\"\n" +
		"s3_bucket = \"vlogs\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	code, stdout, stderr, _ := runCLI("backup", "--config", cfgPath,
		"--date", "2026-10-27", "--dry-run", "--json")
	if code != errs.ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, errs.ExitOK, stderr)
	}
	if strings.Contains(strings.TrimSuffix(stdout, "\n"), "\n") {
		t.Fatalf("stdout %q must be a single JSON line", stdout)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}
	if got["op"] != "backup" || got["status"] != "success" {
		t.Errorf("summary = %v, want op=backup status=success", got)
	}
	if got["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", got["dry_run"])
	}
	parts, ok := got["partitions"].([]any)
	if !ok || len(parts) != 1 || parts[0] != "2026-10-27" {
		t.Errorf("partitions = %v, want [2026-10-27]", got["partitions"])
	}
}

// TestBackupDryRunVLTooOld 覆盖 R-OPS-04：探测端点 404 时退出码 6。
func TestBackupDryRunVLTooOld(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfgBody := "vl_url = \"" + srv.URL + "\"\n" +
		"data_path = \"" + filepath.ToSlash(dir) + "\"\n" +
		"s3_endpoint = \"http://127.0.0.1:9000\"\n" +
		"s3_bucket = \"vlogs\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	code, stdout, stderr, _ := runCLI("backup", "--config", cfgPath,
		"--date", "2026-10-27", "--dry-run", "--json")
	if code != errs.ExitPrecondition {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, errs.ExitPrecondition, stderr)
	}
	if !strings.Contains(stderr, "vl version too old") {
		t.Errorf("stderr %q should mention vl version too old", stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}
	if got["status"] != "error" {
		t.Errorf("status = %v, want error", got["status"])
	}
	if got["error_code"] != float64(errs.ExitPrecondition) {
		t.Errorf("error_code = %v, want %d", got["error_code"], errs.ExitPrecondition)
	}
}
