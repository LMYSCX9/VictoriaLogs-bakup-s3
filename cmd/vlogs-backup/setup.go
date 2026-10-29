package main

import (
	"context"
	"flag"
	"path/filepath"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/report"
	"github.com/vlogs-backup/vlogs-backup/internal/setup"
)

// runSetup 交互式生成 config.toml 与 credentials（R-SU-01~07）。
// 凭据仅经 Prompter 注入，不回显、不落日志（C9）。
func runSetup(e *env, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	_ = fs.String("config", "", "setup 不读取已有配置，生成位置由 --output-dir 决定")
	outputDirFlag := fs.String("output-dir", setup.DefaultOutputDir, "配置生成目录")
	skipCheckFlag := fs.Bool("skip-check", false, "跳过 VL / S3 连通性验证")
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
		OutputDir: dir,
		SkipCheck: *skipCheckFlag,
		Stdout:    out,
		Stderr:    e.stderr,
	}, prompter)

	err := wizard.Run(context.Background())
	s := report.Summary{Op: "setup"}
	if err == nil {
		s.Path = filepath.Join(dir, "config.toml")
	}
	return finishSummary(e, s, err)
}
