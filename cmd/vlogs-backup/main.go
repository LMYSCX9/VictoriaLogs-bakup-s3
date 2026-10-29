// Command vlogs-backup 在 VictoriaLogs 不停机的前提下，
// 把按 UTC 天分区的历史日志备份到 S3，并支持按指定日期在线回灌。
//
// cmd 层只做参数解析、确认交互、退出码映射与输出渲染，业务逻辑一律在 internal/。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// 构建期由 -ldflags 注入。
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

const usageText = `vlogs-backup — VictoriaLogs 不停机备份 / 恢复工具

用法: vlogs-backup [--config <path>] <command> [flags]

命令:
  setup      交互式生成配置文件（默认写入 /etc/vlogs-backup/）
  backup     备份一个或多个日期分区
  restore    在线恢复指定日期分区
  list       列出已提交备份
  verify     校验备份完整性
  delete     删除本地指定日期的分区数据（不可恢复，需确认）
  gc         清理 S3 上无 manifest 的未完成备份前缀
  version    输出版本与构建信息

插件协议（OPS-PLUGIN-1，供平台底座调用）:
  describe   输出插件 manifest JSON（stdout 单个 JSON 对象）
  run        以 task 形态执行子命令: vlogs-backup run <task-name>
             task 名与上述子命令同名（setup 除外）；stdin 收 JSON 参数，
             stdout 输出单个结果信封；非交互确认自动注入

--config 可放在子命令之前（全局形式）或子命令之后；两者同用时子命令形式优先。
运行 "vlogs-backup <command> -h" 查看子命令参数。`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// commands 是全部子命令及其处理函数。
var commands = map[string]func(*env, []string) int{
	"setup":   runSetup,
	"backup":  runBackup,
	"restore": runRestore,
	"list":    runList,
	"verify":  runVerify,
	"delete":  runDelete,
	"gc":      runGC,
}

// run 是 main 的可测试主体，返回进程退出码。
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageText)
		return errs.ExitUsage
	}

	e := &env{stdin: stdin, stdout: stdout, stderr: stderr}
	// 全局 --config 形式（spec R-SU-07）：vlogs-backup --config <path> <command> ...
	// 仅当首个参数就是 --config 时按全局形式摘取；子命令形式随后仍可覆盖（后值优先）。
	if isConfigFlag(args[0]) {
		path, explicit, rest := prescanConfig(args)
		if len(rest) == 0 {
			fmt.Fprintln(stderr, usageText)
			return errs.ExitUsage
		}
		e.cfgPath, e.cfgExplicit = path, explicit
		args = rest
	}

	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "version":
		return runVersion(rest, stdout, stderr)
	case "describe":
		return runDescribe(stdout, stderr)
	case "run":
		// vlogs-backup run <task-name>（插件协议，OPS-PLUGIN-1）；
		// 帮助页（run -h）与缺参处理统一由 runPlugin 兜底。
		return runPlugin(rest, stdin, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usageText)
		return errs.ExitOK
	}

	handler, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(stderr, "vlogs-backup: 未知命令 %q\n\n%s\n", cmd, usageText)
		return errs.ExitUsage
	}

	// 子命令形式 --config（backup --config X ...）；与全局形式同用时以此为准（后值优先）。
	// 子命令参数未显式携带 --config 时保留全局形式（或空默认）的值。
	if path, explicit, rest2 := prescanConfig(rest); explicit {
		e.cfgPath, e.cfgExplicit = path, explicit
		rest = rest2
	}
	return handler(e, rest)
}

// isConfigFlag 判断参数是否为 --config 识别形态（与 prescanConfig 规则一致）。
func isConfigFlag(arg string) bool {
	return arg == "--config" || arg == "-config" ||
		strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "-config=")
}

// prescanConfig 摘取子命令参数最前面的 --config（flag 解析前即可判定是否显式指定）。
// 仅识别出现在最前面的配置文件参数；其余位置交由各子命令的 flagset 处理。
func prescanConfig(args []string) (path string, explicit bool, rest []string) {
	if len(args) == 0 {
		return "", false, args
	}
	switch {
	case args[0] == "--config" || args[0] == "-config":
		if len(args) == 1 {
			return "", true, args // 缺值：交给子命令 flag 解析报错
		}
		return args[1], true, args[2:]
	case strings.HasPrefix(args[0], "--config="), strings.HasPrefix(args[0], "-config="):
		_, v, _ := strings.Cut(args[0], "=")
		return v, true, args[1:]
	default:
		return "", false, args
	}
}

// versionInfo 汇总版本信息，供文本与 JSON 两种渲染。
type versionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

func currentVersion() versionInfo {
	return versionInfo{
		Version:   version,
		Commit:    commit,
		BuildTime: buildTime,
		GoVersion: runtime.Version(),
	}
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "机器可读输出")
	if err := fs.Parse(args); err != nil {
		return errs.ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "vlogs-backup version: 未知参数 %q\n", fs.Arg(0))
		return errs.ExitUsage
	}

	info := currentVersion()
	if *jsonOut {
		b, err := json.Marshal(info)
		if err != nil {
			fmt.Fprintf(stderr, "vlogs-backup: marshal version info: %v\n", err)
			return errs.ExitGeneral
		}
		fmt.Fprintf(stdout, "%s\n", b)
		return errs.ExitOK
	}

	fmt.Fprintf(stdout, "vlogs-backup %s (commit: %s, built: %s, %s)\n",
		info.Version, info.Commit, info.BuildTime, info.GoVersion)
	return errs.ExitOK
}
