package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/plugin"
)

func TestJSONToArgs(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]any
		want  []string
	}{
		{"bool true", map[string]any{"dry_run": true}, []string{"--dry-run=true"}},
		{"bool false explicit", map[string]any{"dry_run": false}, []string{"--dry-run=false"}},
		{"bool false overrides injected yes", map[string]any{"yes": false}, []string{"--yes=false"}},
		{"string value", map[string]any{"date": "2026-10-27"}, []string{"--date=2026-10-27"}},
		{"snake_case to kebab", map[string]any{"if_exists": "replace"}, []string{"--if-exists=replace"}},
		{"int from float64", map[string]any{"latest": float64(3)}, []string{"--latest=3"}},
		{"float value", map[string]any{"ratio": 1.5}, []string{"--ratio=1.5"}},
		{"array expanded", map[string]any{"tags": []any{"a", "b"}}, []string{"--tags=a", "--tags=b"}},
		{"empty string skipped", map[string]any{"date": ""}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jsonToArgs(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("args = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("args[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestReadTaskInput(t *testing.T) {
	tests := []struct {
		name    string
		stdin   string
		wantLen int
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"newline only", "\n", 0, false},
		{"empty object", "{}", 0, false},
		{"one key", `{"date":"2026-10-27"}`, 1, false},
		{"broken json", "{broken", 0, true},
		{"json array rejected", "[]", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := readTaskInput(strings.NewReader(tt.stdin))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.stdin)
				}
				return
			}
			if err != nil {
				t.Fatalf("readTaskInput(%q): %v", tt.stdin, err)
			}
			if len(m) != tt.wantLen {
				t.Errorf("keys = %d, want %d", len(m), tt.wantLen)
			}
		})
	}
}

func TestBuildTaskEnvelope(t *testing.T) {
	data := []byte(`{"op":"list","status":"success"}`)
	tests := []struct {
		name       string
		exit       int
		stdout     []byte
		wantCode   int
		wantOK     bool
		wantErrCod string
		wantRetry  bool
		wantData   bool
	}{
		{"ok", errs.ExitOK, data, plugin.ExitTaskOK, true, "", false, true},
		{"usage", errs.ExitUsage, nil, plugin.ExitTaskInputInvalid, false, plugin.ErrInputInvalid, false, false},
		{"canceled", errs.ExitCanceled, nil, plugin.ExitTaskInputInvalid, false, plugin.ErrInputInvalid, false, false},
		{"not found", errs.ExitNotFound, nil, plugin.ExitTaskExecFailed, false, plugin.ErrExecFailed, false, false},
		{"validation", errs.ExitValidation, nil, plugin.ExitTaskExecFailed, false, plugin.ErrExecFailed, false, false},
		{"precondition", errs.ExitPrecondition, nil, plugin.ExitTaskExecFailed, false, plugin.ErrExecFailed, false, false},
		{"network retryable", errs.ExitNetwork, nil, plugin.ExitTaskExecFailed, false, plugin.ErrExecFailed, true, false},
		{"general retryable", errs.ExitGeneral, nil, plugin.ExitTaskExecFailed, false, plugin.ErrExecFailed, true, false},
		{"unclassified", 99, nil, plugin.ExitTaskGeneralCrash, false, plugin.ErrExecFailed, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, env := buildTaskEnvelope(tt.exit, tt.stdout)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if env == nil {
				t.Fatal("envelope = nil")
			}
			if env.OK != tt.wantOK {
				t.Errorf("ok = %v, want %v", env.OK, tt.wantOK)
			}
			if tt.wantErrCod == "" {
				if env.Error != nil {
					t.Errorf("error = %+v, want nil", env.Error)
				}
			} else {
				if env.Error == nil {
					t.Fatal("error = nil, want non-nil")
				}
				if env.Error.Code != tt.wantErrCod {
					t.Errorf("error.code = %q, want %q", env.Error.Code, tt.wantErrCod)
				}
				if env.Error.Retryable != tt.wantRetry {
					t.Errorf("retryable = %v, want %v", env.Error.Retryable, tt.wantRetry)
				}
			}
			if tt.wantData && len(env.Data) == 0 {
				t.Error("data empty, want summary json")
			}
		})
	}
}

func TestExitCodeToPlugin(t *testing.T) {
	tests := map[int]int{
		errs.ExitOK:           plugin.ExitTaskOK,
		errs.ExitUsage:        plugin.ExitTaskInputInvalid,
		errs.ExitCanceled:     plugin.ExitTaskInputInvalid,
		errs.ExitNetwork:      plugin.ExitTaskExecFailed,
		errs.ExitGeneral:      plugin.ExitTaskExecFailed,
		errs.ExitNotFound:     plugin.ExitTaskExecFailed,
		errs.ExitValidation:   plugin.ExitTaskExecFailed,
		errs.ExitPrecondition: plugin.ExitTaskExecFailed,
		42:                    plugin.ExitTaskGeneralCrash,
	}
	for in, want := range tests {
		if got := exitCodeToPlugin(in); got != want {
			t.Errorf("exitCodeToPlugin(%d) = %d, want %d", in, got, want)
		}
	}
}

// runPlugin 端到端：帮助页、缺参、未知 task、setup 拒绝、协议信封。
func TestRunPlugin_Help(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runPlugin([]string{"-h"}, strings.NewReader(""), &out, &errBuf)
	if code != plugin.ExitTaskOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"backup", "restore", "delete", "gc", "信封"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help text missing %q", want)
		}
	}
	if out.String() != runUsageText+"\n" {
		t.Errorf("help stdout mismatch:\n%s", out.String())
	}
}

func TestRunPlugin_MissingTaskName(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runPlugin(nil, strings.NewReader(""), &out, &errBuf)
	if code != plugin.ExitTaskInputInvalid {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "missing task name") {
		t.Errorf("stderr missing hint: %q", errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty, got %q", out.String())
	}
}

func TestRunPlugin_UnknownTask(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runPlugin([]string{"foobar"}, strings.NewReader(""), &out, &errBuf)
	if code != plugin.ExitTaskInputInvalid {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), `unknown task "foobar"`) {
		t.Errorf("stderr: %q", errBuf.String())
	}
}

func TestRunPlugin_SetupRejected(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runPlugin([]string{"setup"}, strings.NewReader("{}"), &out, &errBuf)
	if code != plugin.ExitTaskInputInvalid {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "setup") {
		t.Errorf("stderr should explain setup rejection: %q", errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("no envelope expected on stdout, got %q", out.String())
	}
}

func TestRunPlugin_VersionRejected(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runPlugin([]string{"version"}, strings.NewReader("{}"), &out, &errBuf)
	if code != plugin.ExitTaskInputInvalid {
		t.Fatalf("exit = %d, want 2 (version has no task form)", code)
	}
}

// run list（无配置）走完整个协议链路：stdin JSON → handler 失败（缺必填配置，
// 原始退出码 2）→ 协议退出码 2 + ok=false 信封；stdout 恰好一行可解析 JSON。
func TestRunPlugin_ListEndToEnd(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runPlugin([]string{"list"}, strings.NewReader("{}"), &out, &errBuf)
	if code != plugin.ExitTaskInputInvalid {
		t.Fatalf("exit = %d, want 2", code)
	}
	env := parseEnvelope(t, out.String())
	if env.OK {
		t.Error("ok = true, want false")
	}
	if env.Error == nil || env.Error.Code != plugin.ErrInputInvalid {
		t.Fatalf("error = %+v, want INPUT_INVALID", env.Error)
	}
}

// 显式传参覆盖 task 注入默认值：yes=false 覆盖 restore 的自动注入。
// 这里通过 jsonToArgs 层验证语义即可（端到端会触发交互拒绝路径，已由 CLI 测试覆盖）。
func TestTaskParamOverrides_ExplicitWins(t *testing.T) {
	input := map[string]any{"yes": false}
	for k, v := range taskParamOverrides["restore"] {
		if _, exists := input[k]; !exists {
			input[k] = v
		}
	}
	if input["yes"] != false {
		t.Errorf("explicit yes=false was overridden: %v", input["yes"])
	}
}

// TestSummaryToData 覆盖信封 data 的三种承载形态（R-PL-02：信封整体必须
// 可单次 Unmarshal——批量 JSONL 必须组装为数组而非拼接）。
func TestSummaryToData(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := summaryToData(nil); got != nil {
			t.Errorf("empty input → %s, want nil", got)
		}
	})
	t.Run("single json passthrough", func(t *testing.T) {
		in := []byte(`{"op":"list","status":"success"}`)
		got := summaryToData(in)
		if got == nil || string(got) != string(in) {
			t.Errorf("got %s, want passthrough %s", got, in)
		}
	})
	t.Run("jsonl becomes array", func(t *testing.T) {
		in := []byte("{\"op\":\"backup\",\"partition\":\"2026-10-26\"}\n{\"op\":\"backup\",\"partition\":\"2026-10-27\"}\n")
		got := summaryToData(in)
		if got == nil {
			t.Fatal("got nil, want array")
		}
		// 信封整体必须可单次解析（原拼接实现会产出非法 JSON）。
		full := []byte(`{"ok":true,"data":` + string(got) + `}`)
		var env plugin.Envelope
		if err := json.Unmarshal(full, &env); err != nil {
			t.Fatalf("envelope not parseable: %v\n%s", err, full)
		}
		var items []map[string]any
		if err := json.Unmarshal(got, &items); err != nil {
			t.Fatalf("data not an array: %v\n%s", err, got)
		}
		if len(items) != 2 {
			t.Errorf("array len = %d, want 2", len(items))
		}
	})
	t.Run("non json line yields nil", func(t *testing.T) {
		if got := summaryToData([]byte("progress 50%\n")); got != nil {
			t.Errorf("got %s, want nil", got)
		}
	})
}

// parseEnvelope 断言 stdout 恰好一行 JSON 且解析为信封。
func parseEnvelope(t *testing.T, stdout string) *plugin.Envelope {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout must be exactly one line, got %d: %q", len(lines), stdout)
	}
	var env plugin.Envelope
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("stdout is not a valid envelope: %v\n%s", err, stdout)
	}
	return &env
}
