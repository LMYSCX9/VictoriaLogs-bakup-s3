// Package plugin 承载插件协议的纯结构体与常量（零 IO 依赖），
// 对齐 manifest-spec v1 与 handshake-protocol OPS-PLUGIN-1。
// 本包禁止 import 任何 IO 包；编解码仅面向内存字节。
package plugin

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// ManifestVersion 是本插件规范版本号，对齐 manifest-spec v1。
const ManifestVersion = 1

// APIVersion 是插件依赖的底座 API 版本声明。
const APIVersion = "1.x"

// Mode 表示插件运行形态（manifest-spec §3 modes）。
type Mode string

const (
	ModeTask   Mode = "task"
	ModeDaemon Mode = "daemon"
)

// Manifest 是插件向平台底座声明自身能力的唯一事实来源
// （manifest-spec v1 顶层结构）。
type Manifest struct {
	ManifestVersion int                   `json:"manifestVersion"`
	Name            string                `json:"name"`
	DisplayName     string                `json:"displayName"`
	Version         string                `json:"version"`
	APIVersion      string                `json:"apiVersion"`
	Description     string                `json:"description,omitempty"`
	Modes           []Mode                `json:"modes"`
	Binary          Binary                `json:"binary"`
	Tasks           []Task                `json:"tasks,omitempty"`
	Service         *Service              `json:"service,omitempty"`
	UI              *UI                   `json:"ui,omitempty"`
	Permissions     []string              `json:"permissions,omitempty"`
	Config          map[string]ConfigItem `json:"config,omitempty"`
}

// Binary 描述插件启动方式（manifest-spec §3.1）。
type Binary struct {
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
}

// Task 是一个一次性任务声明（manifest-spec §3.2）。
type Task struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Timeout     int             `json:"timeout,omitempty"`
}

// Service 描述常驻服务形态（manifest-spec §3.3）。
type Service struct {
	Protocol     string    `json:"protocol,omitempty"`
	ReadyTimeout int       `json:"readyTimeout,omitempty"`
	StopTimeout  int       `json:"stopTimeout,omitempty"`
	Liveness     *Liveness `json:"liveness,omitempty"`
}

// Liveness 是健康检查配置。
type Liveness struct {
	IntervalSeconds  int `json:"intervalSeconds,omitempty"`
	FailureThreshold int `json:"failureThreshold,omitempty"`
}

// UI 是 UI 入口声明（manifest-spec §3.4）。
type UI struct {
	Entries []UIEntry `json:"entries"`
}

// UIEntry 是单个菜单/入口条目。
type UIEntry struct {
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Path   string `json:"path"`
	Target string `json:"target,omitempty"`
	Icon   string `json:"icon,omitempty"`
	Order  int    `json:"order,omitempty"`
}

// ConfigItem 是配置项 schema（manifest-spec §3.6）。
type ConfigItem struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Default     any      `json:"default,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Secret      bool     `json:"secret,omitempty"`
}

// --- 校验辅助 ---

var (
	nameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	taskRegex = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,62}$`)
)

// Validate 校验 manifest 结构自洽（manifest-spec §6 底座校验清单）。
func (m *Manifest) Validate() error {
	if m.ManifestVersion != ManifestVersion {
		return fmt.Errorf("unsupported manifestVersion %d (want %d)", m.ManifestVersion, ManifestVersion)
	}
	if !nameRegex.MatchString(m.Name) {
		return fmt.Errorf("manifest name %q does not match %s", m.Name, nameRegex.String())
	}
	if m.DisplayName == "" {
		return fmt.Errorf("manifest displayName is required")
	}
	if m.Version == "" {
		return fmt.Errorf("manifest version is required")
	}
	if m.APIVersion == "" {
		return fmt.Errorf("manifest apiVersion is required")
	}
	if len(m.Modes) == 0 {
		return fmt.Errorf("manifest modes must contain at least one value")
	}
	hasTask, hasDaemon := false, false
	for _, mo := range m.Modes {
		switch mo {
		case ModeTask:
			hasTask = true
		case ModeDaemon:
			hasDaemon = true
		default:
			return fmt.Errorf("manifest has invalid mode %q", mo)
		}
	}
	if hasTask && len(m.Tasks) == 0 {
		return fmt.Errorf("modes contains task but tasks is empty")
	}
	if m.UI != nil && !hasDaemon {
		return fmt.Errorf("ui is declared but modes does not contain daemon")
	}
	if hasTask {
		seen := make(map[string]struct{}, len(m.Tasks))
		for _, t := range m.Tasks {
			if !taskRegex.MatchString(t.Name) {
				return fmt.Errorf("task name %q does not match %s", t.Name, taskRegex.String())
			}
			if _, dup := seen[t.Name]; dup {
				return fmt.Errorf("duplicate task name %q", t.Name)
			}
			seen[t.Name] = struct{}{}
		}
	}
	if m.Binary.Path == "" {
		return fmt.Errorf("manifest binary.path is required")
	}
	// 校验 config item key 与枚举自洽
	for key, item := range m.Config {
		if !configKeyRegex.MatchString(key) {
			return fmt.Errorf("config key %q does not match %s", key, configKeyRegex.String())
		}
		switch item.Type {
		case "string", "int", "bool", "enum", "stringList":
		default:
			return fmt.Errorf("config %q has invalid type %q", key, item.Type)
		}
		if item.Type == "enum" && len(item.Enum) == 0 {
			return fmt.Errorf("config %q is type=enum but enum list is empty", key)
		}
	}
	return nil
}

var configKeyRegex = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]{1,62}$`)

// --- 握手行解析 ---

// HandshakeLine 是 daemon 模式启动后 stdout 第一行的结构
// （handshake-protocol §4.1）。
type HandshakeLine struct {
	Protocol string
	Network  string
	Addr     string
	Token    string
	PID      int
}

// ParseHandshake 解析握手行字符串；行格式：
//
//	OPS-PLUGIN-1|<protocol>|<network>|<addr>|<token>|<pid>
func ParseHandshake(line string) (*HandshakeLine, error) {
	line = strings.TrimSpace(line)
	const prefix = "OPS-PLUGIN-1|"
	if !strings.HasPrefix(line, prefix) {
		return nil, fmt.Errorf("handshake line missing prefix %q", prefix)
	}
	rest := line[len(prefix):]
	parts := strings.SplitN(rest, "|", 5)
	if len(parts) != 5 {
		return nil, fmt.Errorf("handshake line has %d fields, want 5", len(parts))
	}
	hl := &HandshakeLine{
		Protocol: parts[0],
		Network:  parts[1],
		Addr:     parts[2],
		Token:    parts[3],
	}
	var pid int
	if _, err := fmt.Sscanf(parts[4], "%d", &pid); err != nil {
		return nil, fmt.Errorf("handshake pid %q is not an integer", parts[4])
	}
	hl.PID = pid
	return hl, nil
}

// ValidateListenAddr 校验握手行里的 addr 格式，确保只监听回环
// （handshake-protocol §6 安全要求 1）。
func ValidateListenAddr(network, addr string) error {
	switch network {
	case "tcp":
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("tcp address %q is invalid: %w", addr, err)
		}
		if host != "127.0.0.1" && host != "::1" && host != "localhost" {
			return fmt.Errorf("tcp address %q must bind to loopback", addr)
		}
	case "unix", "pipe":
		// unix socket 路径与 Windows named pipe 名不做额外校验
	default:
		return fmt.Errorf("unknown network %q", network)
	}
	return nil
}
