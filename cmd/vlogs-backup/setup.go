package main

import (
	"context"
	"flag"
	"path/filepath"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/report"
	"github.com/vlogs-backup/vlogs-backup/internal/setup"
)

// runSetup 是顶层入口：根据第一个子参数路由到 file / s3 / legacy 三条路径。
func runSetup(e *env, args []string) int {
	var forceBackend string
	if len(args) > 0 && (args[0] == "file" || args[0] == "s3") {
		// 子命令词汇（file/s3）翻译为向导契约词汇（local/s3）。
		forceBackend = config.StorageTypeLocal
		if args[0] == "s3" {
			forceBackend = config.StorageTypeS3
		}
		args = args[1:]
	}
	return runSetupCore(e, forceBackend, args)
}

// runSetupCore 交互式生成 config.toml 与 credentials（R-SU-01~07）。
// forceBackend 空 → legacy（向导先问后端）；"file"/"s3" → 强制对应后端。
func runSetupCore(e *env, forceBackend string, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	_ = fs.String("config", "", "setup 不读取已有配置，生成位置由 --output-dir 决定")
	outputDirFlag := fs.String("output-dir", setup.DefaultOutputDir, "配置生成目录")
	skipCheckFlag := fs.Bool("skip-check", false, "跳过 VL / 后端连通性验证")
	jsonFlag := fs.Bool("json", false, "机器可读输出（单行 JSON）")
	if err := fs.Parse(args); err != nil {
		return errs.ExitUsage
	}
	if fs.NArg() > 0 {
		return usageErr(e, "setup: unexpected argument %q", fs.Arg(0))
	}
	e.jsonOut = *jsonFlag

	dir := *outputDirFlag
	if dir == "" {
		dir = setup.DefaultOutputDir
	}

	// --json 时把向导交互输出改路由到 stderr，保证 stdout 只有最终一行摘要。
	out := e.stdout
	if e.jsonOut {
		out = e.stderr
	}
	prompter := setup.NewTerminalPrompter(e.stdin, out)
	wizard := setup.NewWizard(setup.Options{
		OutputDir:    dir,
		SkipCheck:    *skipCheckFlag,
		Stdout:       out,
		Stderr:       e.stderr,
		ForceBackend: forceBackend,
	}, prompter)

	err := wizard.Run(context.Background())
	s := report.Summary{Op: "setup"}
	if err == nil {
		s.Path = filepath.Join(dir, "config.toml")
	}
	return finishSummary(e, s, err)
}
