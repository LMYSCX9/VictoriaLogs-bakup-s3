package plugin

import (
	"fmt"
	"os"
)

// 环境变量名（handshake-protocol §2）。
const (
	EnvPlugin       = "OPS_PLUGIN"
	EnvPluginMagic  = "OPS_PLUGIN_MAGIC"
	EnvPluginName   = "OPS_PLUGIN_NAME"
	EnvPluginInst   = "OPS_PLUGIN_INSTANCE"
	EnvPluginData   = "OPS_PLUGIN_DATA_DIR"
	EnvPluginConfig = "OPS_PLUGIN_CONFIG"
	EnvPluginLogLv  = "OPS_PLUGIN_LOG_LEVEL"
	EnvPluginCore   = "OPS_PLUGIN_CORE_ADDR"
	EnvPluginToken  = "OPS_PLUGIN_CORE_TOKEN"
)

// MagicValue 是 OPS_PLUGIN_MAGIC 的固定值（handshake-protocol §2）。
const MagicValue = "OPS-PLUGIN-MAGIC-1"

// IsPluginEnv 报告当前进程是否运行在平台底座拉起的插件环境中。
// 校验规则：OPS_PLUGIN=1 且 OPS_PLUGIN_MAGIC=MagicValue。
// 插件在 serve 模式启动时必须校验这两个值，不符则立即退出（handshake-protocol §6 安全要求 2）。
func IsPluginEnv() bool {
	return os.Getenv(EnvPlugin) == "1" && os.Getenv(EnvPluginMagic) == MagicValue
}

// PluginEnv 是底座注入的插件环境快照（handshake-protocol §2）。
type PluginEnv struct {
	Name     string
	Instance string
	DataDir  string
	Config   string // JSON 字符串，可为 "{}"
	LogLevel string
	CoreAddr string // daemon only
	CoreTok  string // daemon only
}

// ReadPluginEnv 读取底座注入的环境变量快照；
// 当 OPS_PLUGIN!=1 或 MAGIC 不匹配时返回 ErrNotInPluginEnv。
func ReadPluginEnv() (*PluginEnv, error) {
	if !IsPluginEnv() {
		return nil, ErrNotInPluginEnv
	}
	return &PluginEnv{
		Name:     os.Getenv(EnvPluginName),
		Instance: os.Getenv(EnvPluginInst),
		DataDir:  os.Getenv(EnvPluginData),
		Config:   os.Getenv(EnvPluginConfig),
		LogLevel: os.Getenv(EnvPluginLogLv),
		CoreAddr: os.Getenv(EnvPluginCore),
		CoreTok:  os.Getenv(EnvPluginToken),
	}, nil
}

// ErrNotInPluginEnv 表示当前进程不是由插件底座拉起的。
var ErrNotInPluginEnv = fmt.Errorf("not running under ops-plugin base (OPS_PLUGIN/OPS_PLUGIN_MAGIC mismatch)")
