package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/plugin"
)

// taskHandlers 把 task 名路由到原 CLI handler；handler 接收经 JSON→argv 翻译的参数。
// 注册的每个 handler 与原 commands map 保持一致，保证 task 与 CLI 行为等价。
var taskHandlers = map[string]func(*env, []string) int{
	"setup":   runSetup,
	"backup":  runBackup,
	"restore": runRestore,
	"list":    runList,
	"verify":  runVerify,
	"delete":  runDelete,
	"gc":      runGC,
}

// taskParamOverrides 是 task 模式下强制注入的参数，绕过交互式确认与 TTY 检测。
// 键为 task 名，值为追加到 JSON 输入之前/之后的默认值；显式传入的值覆盖这里。
var taskParamOverrides = map[string]map[string]any{
	// restore：task 模式等价于非 TTY，强制 --yes；--force-today 仍需显式传入
	"restore": {"yes": true, "json": true},
	// delete：task 模式等价于非 TTY，强制 --yes；--force-today / --force-no-backup 仍需显式
	"delete": {"yes": true, "json": true},
	// setup：setup 本身是交互向导，task 模式暂不开放（JSON 难以表达交互式多项选择）
	// 保留注册但在 validate 阶段拒绝
}

// runUsageText 是 "vlogs-backup run" 的专属帮助页；各 task 的参数 schema
// 以 describe 输出的 manifest 为权威来源，此处只给入口级说明。
const runUsageText = `vlogs-backup run — 以插件 task 形态执行子命令（OPS-PLUGIN-1）

用法: vlogs-backup run <task-name>

可用 task:
  backup     备份一个或多个日期分区
  restore    在线恢复指定日期分区
  list       列出已提交备份
  verify     校验备份完整性
  delete     删除本地指定日期的分区数据（不可恢复，需确认）
  gc         清理 S3 上无 manifest 的未完成备份前缀

  （setup 是交互式向导，不提供 task 形态）

输入:
  stdin 传一个 JSON 对象，key 为该 task 的参数名（与 CLI flag 同名，
  kebab-case / snake_case 均可），无参数时传 {} 或留空。示例:

    echo '{"date": "2026-10-27", "dry_run": true}' | vlogs-backup run backup

  task 模式自动注入非交互默认值（restore/delete 的 yes=true、json=true），
  显式传入可覆盖。

输出:
  stdout 恰好一个 JSON 信封（handshake-protocol §3.2）:
    成功: {"ok": true,  "data": <摘要 JSON>}
    失败: {"ok": false, "error": {"code": "...", "message": "...", "retryable": false}}
  日志与进度一律走 stderr，不污染 stdout。

退出码: 0 成功 | 1 未分类崩溃 | 2 输入错误 | 3 执行失败 | 4 权限拒绝 | 124 超时

各 task 的完整参数 schema 以 "vlogs-backup describe" 输出为准。`

// runPlugin 是 "run <task-name>" 的完整实现
// （handshake-protocol §3 Task 形态）。
func runPlugin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "vlogs-backup run: missing task name")
		fmt.Fprintln(stderr, runUsageText)
		return plugin.ExitTaskInputInvalid
	}
	// 人类用户的 -h/--help/help：输出帮助页（底座不会传这些值）。
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, runUsageText)
		return plugin.ExitTaskOK
	}
	if len(args) > 1 {
		fmt.Fprintf(stderr, "vlogs-backup run: unexpected argument %q\n", args[1])
		fmt.Fprintln(stderr, runUsageText)
		return plugin.ExitTaskInputInvalid
	}
	taskName := args[0]

	handler, ok := taskHandlers[taskName]
	if !ok {
		fmt.Fprintf(stderr, "vlogs-backup run: unknown task %q; available: %s (see \"vlogs-backup run -h\")\n",
			taskName, strings.Join(registeredTaskNames(), ", "))
		return plugin.ExitTaskInputInvalid
	}

	// setup 是交互式向导，不适合 task 形态（JSON 无法表达多项选择与行内编辑）
	if taskName == "setup" {
		fmt.Fprintln(stderr, "vlogs-backup run: task 'setup' is not available in plugin mode")
		return plugin.ExitTaskInputInvalid
	}

	// 1. 读取 stdin（单个 JSON 对象，可为空 {}）
	input, err := readTaskInput(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "vlogs-backup run %s: read stdin: %v\n", taskName, err)
		return exitCodeToPlugin(errs.ExitUsage)
	}

	// 2. 注入 task 模式默认值（强制非交互确认）
	for k, v := range taskParamOverrides[taskName] {
		if _, exists := input[k]; !exists {
			input[k] = v
		}
	}

	// 3. 强制 json=true，让 handler 输出单行 JSON 到 stdout 以便后续包装
	input["json"] = true

	// 4. JSON → argv
	argv := jsonToArgs(input)

	// 5. 构造 env：stdout 用 buffer 捕获 handler 的 JSON 输出；
	//    stdin 用 /dev/null 等价物防止 handler 内交互读取（task 模式禁止交互）
	var buf bytes.Buffer
	e := &env{
		stdin:  &noopReader{},
		stdout: &buf,
		stderr: stderr,
	}

	// 6. 调用原 handler
	code := handler(e, argv)

	// 7. 构造信封并写 stdout
	pluginCode, envelope := buildTaskEnvelope(code, buf.Bytes())
	if envelope != nil {
		b, merr := json.Marshal(envelope)
		if merr != nil {
			fmt.Fprintf(stderr, "vlogs-backup run %s: marshal envelope: %v\n", taskName, merr)
			return plugin.ExitTaskExecFailed
		}
		fmt.Fprintln(stdout, string(b))
	}
	return pluginCode
}

// registeredTaskNames 返回已注册的 task 名列表（describe 与错误提示共用）。
func registeredTaskNames() []string {
	names := make([]string, 0, len(taskHandlers))
	for k := range taskHandlers {
		names = append(names, k)
	}
	// 固定顺序方便 describe 输出
	order := []string{"backup", "restore", "list", "verify", "delete", "gc", "setup"}
	out := make([]string, 0, len(order))
	for _, n := range order {
		if _, ok := taskHandlers[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// readTaskInput 从 stdin 读取一个 JSON 对象；空输入视为 {}。
func readTaskInput(r io.Reader) (map[string]any, error) {
	reader := bufio.NewReader(r)
	peek, _ := reader.Peek(1)
	if len(peek) == 0 || peek[0] == '\n' || peek[0] == '\r' {
		return map[string]any{}, nil
	}
	b, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("invalid json input: %w", err)
	}
	return m, nil
}

// jsonToArgs 把 task 的 JSON 输入翻译为 flag argv。
// 命名约定：flag 名来自原 CLI（kebab-case，如 if-exists）；
// JSON key 支持 kebab-case 与 snake_case 两种书写，内部统一转 kebab-case 传给 flag。
// bool 一律输出 --flag=<true|false>：保证「显式 false」可覆盖自动注入的 true
// （如 restore 的 yes），不被 flag 默认值吞掉。
func jsonToArgs(input map[string]any) []string {
	var args []string
	for k, v := range input {
		flag := toFlagName(k)
		switch val := v.(type) {
		case bool:
			args = append(args, "--"+flag+"="+strconv.FormatBool(val))
		case string:
			if val != "" {
				args = append(args, "--"+flag+"="+val)
			}
		case float64:
			// JSON 数字默认解析为 float64
			if val == float64(int(val)) {
				args = append(args, fmt.Sprintf("--%s=%d", flag, int(val)))
			} else {
				args = append(args, fmt.Sprintf("--%s=%s", flag, fmt.Sprintf("%g", val)))
			}
		case int:
			args = append(args, fmt.Sprintf("--%s=%d", flag, val))
		case []any:
			// 数组类型逐项展开（--from-date、--to-date 等场景）
			for _, item := range val {
				args = append(args, fmt.Sprintf("--%s=%v", flag, item))
			}
		}
	}
	return args
}

// toFlagName 把 JSON key（支持 snake_case 与 kebab-case）统一转为 flag 的 kebab-case。
func toFlagName(key string) string {
	return strings.ReplaceAll(key, "_", "-")
}

// noopReader 实现 io.Reader，始终返回 EOF（用于禁止 task 模式下 handler 内部交互式读 stdin）。
type noopReader struct{}

func (*noopReader) Read(p []byte) (int, error) { return 0, io.EOF }

// buildTaskEnvelope 把原 CLI handler 的退出码与捕获的 stdout 输出翻译为插件信封
// （handshake-protocol §3.2 退出码映射）。
// data 承载规则：单个 JSON 原样透传；批量命令（backup/delete --json）的 JSONL
// 输出逐行解析后组装为 JSON 数组，保证信封整体始终是合法 JSON。
// envelope 为 nil 表示 handler 已经直接写出了信封（不应发生）。
func buildTaskEnvelope(exitCode int, handlerStdout []byte) (int, *plugin.Envelope) {
	out := bytes.TrimSpace(handlerStdout)
	data := summaryToData(out)

	switch exitCode {
	case errs.ExitOK:
		return plugin.ExitTaskOK, &plugin.Envelope{OK: true, Data: data}
	case errs.ExitUsage:
		return plugin.ExitTaskInputInvalid, &plugin.Envelope{
			OK: false, Error: &plugin.ErrorInfo{Code: plugin.ErrInputInvalid, Message: extractErrMsg(out), Retryable: false},
		}
	case errs.ExitCanceled:
		return plugin.ExitTaskInputInvalid, &plugin.Envelope{
			OK: false, Error: &plugin.ErrorInfo{Code: plugin.ErrInputInvalid, Message: extractErrMsg(out), Retryable: false},
		}
	case errs.ExitNotFound, errs.ExitValidation, errs.ExitPrecondition:
		return plugin.ExitTaskExecFailed, &plugin.Envelope{
			OK: false, Error: &plugin.ErrorInfo{Code: plugin.ErrExecFailed, Message: extractErrMsg(out), Retryable: false},
		}
	case errs.ExitNetwork, errs.ExitGeneral:
		return plugin.ExitTaskExecFailed, &plugin.Envelope{
			OK: false, Error: &plugin.ErrorInfo{Code: plugin.ErrExecFailed, Message: extractErrMsg(out), Retryable: true},
		}
	default:
		return plugin.ExitTaskGeneralCrash, &plugin.Envelope{
			OK: false, Error: &plugin.ErrorInfo{Code: plugin.ErrExecFailed, Message: extractErrMsg(out), Retryable: false},
		}
	}
}

// exitCodeToPlugin 独立版映射（供 describe 输出前的早期失败使用，此时无 envelope）。
func exitCodeToPlugin(code int) int {
	switch code {
	case errs.ExitOK:
		return plugin.ExitTaskOK
	case errs.ExitUsage, errs.ExitCanceled:
		return plugin.ExitTaskInputInvalid
	case errs.ExitNetwork, errs.ExitGeneral, errs.ExitNotFound, errs.ExitValidation, errs.ExitPrecondition:
		return plugin.ExitTaskExecFailed
	default:
		return plugin.ExitTaskGeneralCrash
	}
}

// extractErrMsg 从 handler 输出的 report.Summary JSON 中提取错误消息。
// handler 在 finishSummary 中会把 error_code 之外的信息放在 stderr，
// stdout 的 JSON 里只有 status/error_code。这里返回友好描述。
func extractErrMsg(out []byte) string {
	if len(out) == 0 {
		return ""
	}
	var summary struct {
		Status    string `json:"status"`
		ErrorCode int    `json:"error_code"`
		Error     string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(out, &summary); err != nil {
		return string(out)
	}
	if summary.Error != "" {
		return summary.Error
	}
	if summary.Status == "error" {
		return fmt.Sprintf("exit_code=%d", summary.ErrorCode)
	}
	return ""
}

// summaryToData 把 handler 捕获的 stdout 翻译为信封 data 字段：
//   - 空 → nil；
//   - 合法单个 JSON → 原样透传；
//   - JSONL（批量 backup/delete --json 每天一行）→ 逐行解析组装为 JSON 数组，
//     保证 {"ok":...,"data":[...]} 整体可被调用方单次 Unmarshal。
//
// 两类解析都失败时返回 nil（错误信息已在 stderr，信封靠 error 字段表达）。
func summaryToData(out []byte) json.RawMessage {
	if len(out) == 0 {
		return nil
	}
	if json.Valid(out) {
		return json.RawMessage(out)
	}
	var arr []json.RawMessage
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return nil // 混入非 JSON 行（交互污染等），放弃 data，错误走 stderr
		}
		arr = append(arr, json.RawMessage(line))
	}
	if len(arr) == 0 {
		return nil
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return nil
	}
	return b
}
