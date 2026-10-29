package errs

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestExitCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"plain error", io.EOF, ExitGeneral},
		{"untyped", Untyped(io.EOF, "copy failed"), ExitGeneral},
		{"usage", Usage("missing --date"), ExitUsage},
		{"usagef", Usagef(io.EOF, "bad flag"), ExitUsage},
		{"network", Network(io.EOF, "vl unreachable"), ExitNetwork},
		{"not found", NotFound(io.EOF, "no manifest"), ExitNotFound},
		{"validation", Validation(io.EOF, "size mismatch"), ExitValidation},
		{"precondition", Precondition(io.EOF, "disk full"), ExitPrecondition},
		{"canceled", Canceled("user aborted"), ExitCanceled},
		{"wrapped chain", fmt.Errorf("outer: %w", Precondition(io.EOF, "lock held")), ExitPrecondition},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrorChain(t *testing.T) {
	t.Parallel()

	base := io.EOF
	err := Network(base, "vl unreachable")

	// 错误信息包含上下文与原始错误
	if got, want := err.Error(), "vl unreachable: EOF"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// Unwrap 保留链路，errors.Is 可判定
	if !errors.Is(err, base) {
		t.Error("errors.Is should find wrapped io.EOF")
	}
	// 无 cause 时消息不带冒号后缀
	if got, want := Usage("missing --date").Error(), "missing --date"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
