// Package errs 定义工具的固定退出码契约与错误分类。
// 退出码语义见设计文档 §11.2，不得复用或新增含义。
package errs

import (
	"errors"
	"fmt"
)

// 固定退出码，供 cron / 告警消费。
const (
	ExitOK           = 0 // 成功
	ExitGeneral      = 1 // 未分类错误 / 回滚失败
	ExitUsage        = 2 // 参数或配置错误
	ExitNetwork      = 3 // 远端不可达 / 网络失败
	ExitNotFound     = 4 // 目标不存在
	ExitValidation   = 5 // 校验失败
	ExitPrecondition = 6 // 前置条件不满足
	ExitCanceled     = 7 // 用户取消
)

// Error 是带退出码分类的错误，所有对外暴露的失败都应经由本类型。
type Error struct {
	Code  int
	msg   string
	cause error
}

func (e *Error) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + ": " + e.cause.Error()
}

func (e *Error) Unwrap() error { return e.cause }

// Untyped 构造未分类错误（退出码 1）。
func Untyped(err error, format string, args ...any) *Error {
	return &Error{Code: ExitGeneral, msg: fmt.Sprintf(format, args...), cause: err}
}

// Usage 构造参数或配置错误（退出码 2）。
func Usage(format string, args ...any) *Error {
	return &Error{Code: ExitUsage, msg: fmt.Sprintf(format, args...)}
}

// Usagef 包装底层错误的参数或配置错误（退出码 2）。
func Usagef(err error, format string, args ...any) *Error {
	return &Error{Code: ExitUsage, msg: fmt.Sprintf(format, args...), cause: err}
}

// Network 构造远端不可达错误（退出码 3）。
func Network(err error, format string, args ...any) *Error {
	return &Error{Code: ExitNetwork, msg: fmt.Sprintf(format, args...), cause: err}
}

// NotFound 构造目标不存在错误（退出码 4）。
func NotFound(err error, format string, args ...any) *Error {
	return &Error{Code: ExitNotFound, msg: fmt.Sprintf(format, args...), cause: err}
}

// Validation 构造校验失败错误（退出码 5）。
func Validation(err error, format string, args ...any) *Error {
	return &Error{Code: ExitValidation, msg: fmt.Sprintf(format, args...), cause: err}
}

// Precondition 构造前置条件不满足错误（退出码 6）。
func Precondition(err error, format string, args ...any) *Error {
	return &Error{Code: ExitPrecondition, msg: fmt.Sprintf(format, args...), cause: err}
}

// Canceled 构造用户取消错误（退出码 7）。
func Canceled(format string, args ...any) *Error {
	return &Error{Code: ExitCanceled, msg: fmt.Sprintf(format, args...)}
}

// ExitCode 从错误链中提取退出码；无 *Error 时按未分类处理（退出码 1）。
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ExitGeneral
}
