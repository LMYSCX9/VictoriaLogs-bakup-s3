package setup

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestReadEditedLine 表驱动覆盖行编辑器核心行为（R-SU-02 验收：^H 与 ^? 均删除）。
func TestReadEditedLine(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		echo    bool
		want    string
		wantErr error
		outHas  []string // 回显输出必须包含的片段
		outNot  []string // 回显输出必须不包含的片段
	}{
		{
			name:   "plain input",
			in:     "abc\r",
			echo:   true,
			want:   "abc",
			outHas: []string{"abc\r\n"},
		},
		{
			name:   "backspace 0x08 erases",
			in:     "ab\x08c\r",
			echo:   true,
			want:   "ac",
			outHas: []string{"\b \b"},
		},
		{
			name:   "backspace 0x7F erases identically",
			in:     "ab\x7fc\r",
			echo:   true,
			want:   "ac",
			outHas: []string{"\b \b"},
		},
		{
			name:   "backspace on empty line ignored",
			in:     "\x08\x08ab\r",
			echo:   true,
			want:   "ab",
			outNot: []string{"\b"},
		},
		{
			name:   "ctrl-u clears whole line",
			in:     "ab\x15cd\r",
			echo:   true,
			want:   "cd",
			outHas: []string{"ab\b \b\b \bcd\r\n"},
		},
		{
			name:    "ctrl-c interrupts",
			in:      "ab\x03",
			echo:    true,
			want:    "",
			wantErr: errInterrupt,
		},
		{
			name:    "ctrl-d on empty line is EOF",
			in:      "\x04",
			echo:    true,
			want:    "",
			wantErr: io.EOF,
		},
		{
			name: "ctrl-d mid-line ignored",
			in:   "ab\x04cd\r",
			echo: true,
			want: "abcd",
		},
		{
			name:   "cjk backspace erases 2 columns",
			in:     "中\x08x\r",
			echo:   true,
			want:   "x",
			outHas: []string{"中\b \b\b \bx\r\n"},
		},
		{
			name: "invalid utf-8 passes through",
			in:   "\xC0\x80a\r",
			echo: true,
			want: "\xC0\x80a",
		},
		{
			name: "lf terminates line",
			in:   "ab\n",
			echo: true,
			want: "ab",
		},
		{
			name: "tab ignored",
			in:   "a\tb\r",
			echo: true,
			want: "ab",
		},
		{
			name:   "echo=false hides input",
			in:     "secret\r",
			echo:   false,
			want:   "secret",
			outHas: []string{"\r\n"},
			outNot: []string{"secret", "\b"},
		},
		{
			name: "extra backspaces no-op",
			in:   "abc\x08\x08\x08\x08d\r",
			echo: true,
			want: "d",
		},
		{
			name:    "eof without newline discards partial line",
			in:      "ab",
			echo:    true,
			want:    "",
			wantErr: io.EOF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := readEditedLine(strings.NewReader(tt.in), &out, tt.echo)
			if err != tt.wantErr {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("line = %q, want %q", got, tt.want)
			}
			o := out.String()
			for _, frag := range tt.outHas {
				if !strings.Contains(o, frag) {
					t.Errorf("output %q missing %q", o, frag)
				}
			}
			for _, frag := range tt.outNot {
				if strings.Contains(o, frag) {
					t.Errorf("output %q should not contain %q", o, frag)
				}
			}
		})
	}
}

// TestTerminalPrompterPipe 覆盖非交互管道路径：MakeRaw 必然失败，降级为缓冲读法；
// Line 空输入回落默认值，Password 走同一读法，管道不算交互终端。
func TestTerminalPrompterPipe(t *testing.T) {
	p := NewTerminalPrompter(strings.NewReader("\n"), io.Discard)
	got, err := p.Line("label", "def")
	if err != nil || got != "def" {
		t.Errorf("Line empty = %q, %v; want def, nil", got, err)
	}

	p = NewTerminalPrompter(strings.NewReader("hello\n"), io.Discard)
	got, err = p.Line("label", "def")
	if err != nil || got != "hello" {
		t.Errorf("Line = %q, %v; want hello, nil", got, err)
	}

	p = NewTerminalPrompter(strings.NewReader("pw\n"), io.Discard)
	got, err = p.Password("secret")
	if err != nil || got != "pw" {
		t.Errorf("Password = %q, %v; want pw, nil", got, err)
	}

	if p.Interactive() {
		t.Errorf("pipe stdin should not be interactive")
	}
}

// TestTerminalPrompterLineWritesPrompt 确认提示写入 stdout 且默认值参与展示。
func TestTerminalPrompterLineWritesPrompt(t *testing.T) {
	out := &bytes.Buffer{}
	p := NewTerminalPrompter(strings.NewReader("\n"), out)
	if _, err := p.Line("VL 地址", "http://x"); err != nil {
		t.Fatalf("Line: %v", err)
	}
	if !strings.Contains(out.String(), "VL 地址 [http://x]: ") {
		t.Errorf("prompt = %q, want label with default", out.String())
	}
}
