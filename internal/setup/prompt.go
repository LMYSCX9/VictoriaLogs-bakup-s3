package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"golang.org/x/term"
)

// errInterrupt 表示用户按 Ctrl-C 中止输入；向导层将其翻译为取消（退出码 7）。
var errInterrupt = errors.New("interrupted by ctrl-c")

// Prompter 是向导的输入抽象（设计文档 §8.2）：Line 读一行文本并展示默认值，
// Password 读一个不回显的秘密值；Interactive 报告 stdin 是否为可交互终端。
// 测试注入脚本化实现，生产注入 TerminalPrompter。
type Prompter interface {
	Line(prompt, def string) (string, error)
	Password(prompt string) (string, error)
	Interactive() bool
}

// TerminalPrompter 是面向真实终端的 Prompter 实现。
// stdin 为字符设备时判定为交互终端；交互输入走 raw 模式行编辑器，使 ^H 与 ^?
// 都执行退格（R-SU-02）；秘密输入不回显（R-SU-03）。非交互终端由 Wizard 以
// 退出码 2 拒绝（R-SU-06）。
type TerminalPrompter struct {
	in          *bufio.Reader
	stdout      io.Writer
	file        *os.File // stdin 为终端句柄时非 nil，据此进出 raw 模式
	interactive bool
}

// NewTerminalPrompter 基于给定 stdin 构造；interactive 由 stdin 是否为字符设备判定。
func NewTerminalPrompter(stdin io.Reader, stdout io.Writer) *TerminalPrompter {
	p := &TerminalPrompter{in: bufio.NewReader(stdin), stdout: stdout}
	if f, ok := stdin.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			p.file = f
			p.interactive = true
		}
	}
	return p
}

// Line 输出提示并读取一行；空输入回落到默认值 def。
// 提示在进入 raw 模式前输出（cooked 模式下正常刷新），随后交互终端走行编辑器。
func (p *TerminalPrompter) Line(prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.stdout, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(p.stdout, "%s: ", prompt)
	}
	line, err := p.read(true)
	if err != nil {
		return "", err
	}
	if line == "" {
		return def, nil
	}
	return line, nil
}

// Password 输出提示并读取秘密值：交互终端走行编辑器（echo=false，不回显，
// R-SU-03）；编辑器在回车时总会补发换行，无需额外补偿输出。
// 非交互路径仅在测试脚本喂入时可达——Wizard 在非 TTY 时早已拒绝进入询问流程。
func (p *TerminalPrompter) Password(prompt string) (string, error) {
	fmt.Fprintf(p.stdout, "%s: ", prompt)
	return p.read(false)
}

// read 读一行输入。交互终端优先走 raw 行编辑器（MakeRaw 失败时降级为 canonical
// 缓冲读法，保证降级路径可用而非报错退出）；非交互路径走缓冲读法。
func (p *TerminalPrompter) read(echo bool) (string, error) {
	if p.file != nil {
		if line, ok, err := p.tryRaw(echo); ok {
			return line, err
		}
	}
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return trimEOL(line), nil
}

// tryRaw 尝试以 raw 模式读取一行；ok=false 表示无法进入 raw 模式，调用方降级。
func (p *TerminalPrompter) tryRaw(echo bool) (line string, ok bool, err error) {
	fd := int(p.file.Fd())
	old, merr := term.MakeRaw(fd)
	if merr != nil {
		return "", false, nil
	}
	defer term.Restore(fd, old)
	line, err = readEditedLine(p.file, p.stdout, echo)
	return line, true, err
}

// readEditedLine 是自绘行编辑器（raw 模式下运行）：从 r 逐字节读取并按需回显到
// out，支持行内编辑——退格键 ^H(0x08) 与 ^?(0x7F) 均删除、Ctrl-U 清空整行、
// Ctrl-C 中止、Ctrl-D 空行视为 EOF（行中忽略）。raw 模式下 OPOST 关闭，所有
// 换行必须显式发 "\r\n"。echo=false 时不回显输入字符（密码路径），但回车换行
// 仍输出以保持提示行推进。该函数不管理终端状态，便于对纯字节流做表驱动测试。
func readEditedLine(r io.Reader, out io.Writer, echo bool) (string, error) {
	var (
		line    []byte // 已确认的输入（合法 UTF-8 序列按 rune 追加，非法字节原样保留）
		pending []byte // 未完成的 UTF-8 多字节序列（已读未回显）
	)
	buf := make([]byte, 1)

	// erase 擦除 n 个显示列（echo=false 时不产生输出）。
	erase := func(cols int) {
		for i := 0; echo && i < cols; i++ {
			fmt.Fprint(out, "\b \b")
		}
	}
	// backspace 删除最后一个输入单元：未回显的 pending 字节直接丢弃；
	// 已回显内容按最后一 rune 的显示宽度擦除（非法序列按字节数逐列擦除）。
	backspace := func() {
		if len(pending) > 0 {
			pending = pending[:len(pending)-1]
			return
		}
		if len(line) == 0 {
			return
		}
		i := len(line) - 1
		for i > 0 && line[i]&0xC0 == 0x80 {
			i--
		}
		r, size := utf8.DecodeRune(line[i:])
		if size == len(line)-i {
			erase(runeColumns(r))
		} else {
			erase(len(line) - i)
		}
		line = line[:i]
	}
	// clearLine 是 Ctrl-U：擦除全部已回显内容并复位缓冲。
	clearLine := func() {
		for i := 0; echo && i < len(line); {
			r, size := utf8.DecodeRune(line[i:])
			erase(runeColumns(r))
			i += size
		}
		line = line[:0]
		pending = pending[:0]
	}

	for {
		n, rerr := r.Read(buf)
		if n == 0 {
			if rerr != nil {
				return "", rerr
			}
			continue
		}
		b := buf[0]
		switch b {
		case 0x03: // Ctrl-C：中止
			fmt.Fprint(out, "\r\n")
			return "", errInterrupt
		case 0x04: // Ctrl-D：空行 EOF，行中忽略
			if len(line) == 0 && len(pending) == 0 {
				fmt.Fprint(out, "\r\n")
				return "", io.EOF
			}
		case 0x08, 0x7F: // 退格：^H 与 ^? 等价（兼容不同 SSH 客户端键码）
			backspace()
		case 0x15: // Ctrl-U：清空整行
			clearLine()
		case '\r', '\n': // 回车：结束（raw 模式必须显式发 \r\n）
			fmt.Fprint(out, "\r\n")
			return string(line), nil
		default:
			if b < 0x20 {
				continue // 其余控制字节（Tab、ESC 等）忽略，避免干扰行编辑
			}
			if exp := utf8Len(b); len(pending) == 0 && exp > 1 {
				pending = append(pending[:0], b) // 多字节序列首字节，暂不回显
				continue
			}
			if len(pending) > 0 {
				pending = append(pending, b)
				if len(pending) < utf8Len(pending[0]) {
					continue // 序列未完整，继续收集
				}
				// 序列完整：合法与否均原样入缓冲并回显（不猜测用户输入）
				line = append(line, pending...)
				if echo {
					_, _ = out.Write(pending)
				}
				pending = pending[:0]
				continue
			}
			// 单字节：ASCII 或孤立非法字节
			line = append(line, b)
			if echo {
				_, _ = out.Write([]byte{b})
			}
		}
	}
}

// utf8Len 返回以 b 为首字节的 UTF-8 序列期望长度；0 表示 b 不可能是合法首字节。
func utf8Len(b byte) int {
	switch {
	case b < 0x80:
		return 1
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	default:
		return 0
	}
}

// runeColumns 返回 rune 在终端的显示列宽：CJK/全角区段为 2，其余为 1。
// 仅用于退格擦除对齐；区段表为 go-runewidth 的常用子集，避免引入第三方依赖。
func runeColumns(r rune) int {
	switch {
	case r < 0x1100:
		return 1
	case r <= 0x115F, // Hangul Jamo 初声
		r == 0x2329 || r == 0x232A,
		r >= 0x2E80 && r <= 0x303E,   // CJK 部首～CJK 符号（0x303F 半角除外）
		r >= 0x3041 && r <= 0x33FF,   // 假名～CJK 兼容字符
		r >= 0x3400 && r <= 0x4DBF,   // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK 统一表意文字
		r >= 0xA000 && r <= 0xA4CF,   // Yi 音节
		r >= 0xAC00 && r <= 0xD7A3,   // Hangul 音节
		r >= 0xF900 && r <= 0xFAFF,   // CJK 兼容表意
		r >= 0xFE30 && r <= 0xFE4F,   // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60,   // 全角形式
		r >= 0xFFE0 && r <= 0xFFE6,   // 全角符号
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B～
		return 2
	default:
		return 1
	}
}

// Interactive 报告 stdin 是否为交互终端。
func (p *TerminalPrompter) Interactive() bool { return p.interactive }

func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
