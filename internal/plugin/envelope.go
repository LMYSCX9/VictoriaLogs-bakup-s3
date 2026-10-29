package plugin

import "encoding/json"

// Envelope 是 Task 模式 stdout 输出的单个 JSON 信封
// （handshake-protocol §3.2）。
//
// 成功：{"ok": true, "data": {...}}
// 失败：{"ok": false, "error": {...}}
type Envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *ErrorInfo      `json:"error,omitempty"`
}

// ErrorInfo 是信封失败分支的 error 字段。
type ErrorInfo struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// 预定义错误码（handshake-protocol §3.2 保留字）。
const (
	ErrTimeout          = "TIMEOUT"
	ErrInputInvalid     = "INPUT_INVALID"
	ErrPermissionDenied = "PERMISSION_DENIED"
	ErrExecFailed       = "EXEC_FAILED"
)

// --- 退出码映射 ---

// TaskExitCode 是插件 task 形态的退出码语义（handshake-protocol §3.3）。
const (
	ExitTaskOK           = 0
	ExitTaskGeneralCrash = 1
	ExitTaskInputInvalid = 2
	ExitTaskExecFailed   = 3
	ExitTaskPermission   = 4
	ExitTaskTimeout      = 124
)
