package main

import (
	"context"
	"flag"
	"strings"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/report"
	"github.com/vlogs-backup/vlogs-backup/internal/restore"
)

// runRestore 在线恢复单个分区（R-RS-01~10）。
// 确认矩阵在配置加载之前完成（C10 / R-RS-08）：非交互必须 --yes，
// UTC 今天分区必须 --force-today，交互终端需逐项确认。
func runRestore(e *env, args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	cfgFlag := fs.String("config", "", "配置文件路径（默认 "+defaultConfigPath+"）")
	dateFlag := fs.String("date", "", "恢复分区，YYYY-MM-DD（默认 UTC 昨天）")
	onExistingFlag := fs.String("on-existing", restore.OnExistingKeep, "目标分区已存在时：keep|delete")
	deepFlag := fs.Bool("deep", false, "下载后逐文件比对 sha256")
	forceTodayFlag := fs.Bool("force-today", false, "允许恢复 UTC 今天分区（造成实时写入缺口）")
	allowNodeMismatchFlag := fs.Bool("allow-node-mismatch", false, "放行 manifest 中 cluster/node 不一致的备份")
	yesFlag := fs.Bool("yes", false, "跳过交互确认（非交互环境必须显式给出）")
	dryRunFlag := fs.Bool("dry-run", false, "只演练：探测 VL 能力并打印计划，不下载不替换")
	jsonFlag := fs.Bool("json", false, "机器可读输出（单行 JSON）")
	if err := fs.Parse(args); err != nil {
		return errs.ExitUsage
	}
	if fs.NArg() > 0 {
		return usageErr(e, "restore: unexpected argument %q", fs.Arg(0))
	}
	e.jsonOut = *jsonFlag
	applyConfigFlag(fs, e, *cfgFlag)

	switch *onExistingFlag {
	case restore.OnExistingKeep, restore.OnExistingDelete:
	default:
		return usageErr(e, "--on-existing must be %q or %q, got %q",
			restore.OnExistingKeep, restore.OnExistingDelete, *onExistingFlag)
	}

	day := *dateFlag
	if strings.TrimSpace(day) == "" {
		day = yesterdayUTC()
	}
	partition, err := parseDay("date", day)
	if err != nil {
		return usageErr(e, "%v", err)
	}

	if !*dryRunFlag {
		code, ok := confirmRestore(e, partition, *yesFlag, *forceTodayFlag)
		if !ok {
			return code
		}
	}

	cfg, err := loadConfigFor(context.Background(), e,
		config.FieldValuePath, config.FieldDataPath, config.FieldS3Endpoint, config.FieldS3Bucket)
	if err != nil {
		return fail(e, err)
	}
	ctx, cancel := commandContext(cfg)
	defer cancel()

	rt, err := buildRuntime(ctx, e, cfg, "restore", runtimeNeeds{
		vl:    true,
		s3:    !*dryRunFlag,
		locks: !*dryRunFlag,
	})
	if err != nil {
		return fail(e, err)
	}

	start := time.Now()
	s := baseSummary(cfg, "restore")
	s.Partition = formatDay(partition)
	s.DryRun = *dryRunFlag
	if *dryRunFlag {
		s.ElapsedMS = time.Since(start).Milliseconds()
		return finishSummary(e, s, rt.vl.Probe(ctx))
	}

	// R-OPS-04：恢复路径必须显式探测 VL 能力，端点缺失时早期失败。
	if err := rt.vl.Probe(ctx); err != nil {
		return finishSummary(e, s, err)
	}

	job, err := restore.NewJob(rt.vl, rt.store, rt.locks, restore.Options{
		Cluster:           cfg.Cluster,
		Node:              cfg.Node,
		S3Prefix:          cfg.S3Prefix,
		DataPath:          cfg.DataPath,
		RetentionDays:     cfg.VLRetentionDays,
		Concurrency:       cfg.Concurrency,
		Deep:              *deepFlag,
		OnExisting:        *onExistingFlag,
		ForceToday:        *forceTodayFlag,
		AllowNodeMismatch: *allowNodeMismatchFlag,
		DiskCheckFactor:   cfg.DiskCheckFactor,
		Logger:            rt.logger,
	})
	if err != nil {
		return finishSummary(e, s, err)
	}

	res, runErr := job.Run(ctx, partition)
	if res.Files > 0 {
		s.Files = int64(res.Files)
	}
	s.Bytes = res.Bytes
	s.SmokeQuery = res.Smoke
	s.ReplacedPath = res.ReplacedPath
	s.CrossNode = res.CrossNode
	s.ElapsedMS = res.ElapsedMS
	if s.ElapsedMS == 0 {
		s.ElapsedMS = time.Since(start).Milliseconds()
	}
	return finishSummary(e, s, runErr)
}

// confirmRestore 执行恢复的确认矩阵；ok=false 时返回值即退出码。
func confirmRestore(e *env, partition string, yes, forceToday bool) (code int, ok bool) {
	tty := stdinIsTTY(e)
	isToday := partition == utcToday()

	if isToday && !forceToday {
		// R-RS-08：恢复今天分区必须显式授权，交互与否都不例外。
		return usageErr(e,
			"restore: partition %s is the UTC today partition; pass --force-today to proceed",
			formatDay(partition)), false
	}
	if !tty && !yes {
		// C10 / R-RS-08：非交互环境必须显式 --yes。
		return usageErr(e,
			"restore: refuses to run without --yes in a non-interactive environment"), false
	}
	if !tty {
		return errs.ExitOK, true
	}
	if isToday {
		// 今天分区：输入完整日期串才继续（即使已给 --yes）。
		if err := confirmPartition(e, partition); err != nil {
			return finishSummary(e, restoreCancelSummary(partition), err), false
		}
		return errs.ExitOK, true
	}
	if yes {
		return errs.ExitOK, true
	}
	if err := confirmYes(e, partition); err != nil {
		return finishSummary(e, restoreCancelSummary(partition), err), false
	}
	return errs.ExitOK, true
}

// restoreCancelSummary 构造确认取消时的摘要（配置尚未加载，cluster/node 留空）。
func restoreCancelSummary(partition string) report.Summary {
	return report.Summary{Op: "restore", Partition: formatDay(partition)}
}
