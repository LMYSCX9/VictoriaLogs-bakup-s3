package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/backup"
	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// runBackup 是顶层入口：根据第一个子参数路由到 file / s3 / legacy 三条路径。
func runBackup(e *env, args []string) int {
	if len(args) > 0 && args[0] == "file" {
		return runBackupCore(e, config.StorageTypeLocal, args[1:])
	}
	if len(args) > 0 && args[0] == "s3" {
		return runBackupCore(e, config.StorageTypeS3, args[1:])
	}
	// legacy：按 config 里的 storage_type 自动路由
	fmt.Fprintln(e.stderr, deprecatedMsg("backup"))
	e.deprecated = true
	return runBackupCore(e, "", args)
}

// runBackupCore 是备份核心逻辑；requireStorageType="" 表示按 cfg 自动选后端（legacy），
// "local" / "s3" 表示子命令强制指定。
func runBackupCore(e *env, requireStorageType string, args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	cfgFlag := fs.String("config", "", "配置文件路径（默认 "+defaultConfigPath+"）")
	dateFlag := fs.String("date", "", "备份单日分区，YYYY-MM-DD（默认 UTC 昨天）")
	fromFlag := fs.String("from", "", "批量起始日期，YYYY-MM-DD（含端点）")
	toFlag := fs.String("to", "", "批量结束日期，YYYY-MM-DD（含端点）")
	latestFlag := fs.Int("latest", 0, "备份最近 N 天（含 UTC 今天）")
	ifExistsFlag := fs.String("if-exists", backup.IfExistsSkip, "目标日期已有已提交备份时：skip|replace")
	keepSnapshotFlag := fs.Bool("keep-snapshot", false, "保留快照用于排障（占用磁盘，需人工清理）")
	dryRunFlag := fs.Bool("dry-run", false, "只演练：探测 VL 能力并列出计划分区，不上传")
	jsonFlag := fs.Bool("json", false, "机器可读输出（单行 JSON）")
	if err := fs.Parse(args); err != nil {
		return errs.ExitUsage
	}
	if fs.NArg() > 0 {
		return usageErr(e, "backup: unexpected argument %q", fs.Arg(0))
	}
	e.jsonOut = *jsonFlag
	applyConfigFlag(fs, e, *cfgFlag)

	if *ifExistsFlag != backup.IfExistsSkip && *ifExistsFlag != backup.IfExistsReplace {
		return usageErr(e, "--if-exists must be %q or %q, got %q",
			backup.IfExistsSkip, backup.IfExistsReplace, *ifExistsFlag)
	}
	partitions, err := resolveDates(*dateFlag, *fromFlag, *toFlag, *latestFlag, time.Now().UTC())
	if err != nil {
		return usageErr(e, "%v", err)
	}

	cfg, err := loadConfigFor(context.Background(), e, requireStorageType,
		config.FieldValuePath, config.FieldDataPath)
	if err != nil {
		return fail(e, err)
	}
	ctx, cancel := commandContext(cfg)
	defer cancel()

	rt, err := buildRuntime(ctx, e, cfg, "backup", runtimeNeeds{
		vl:    true,
		s3:    !*dryRunFlag,
		locks: !*dryRunFlag,
	})
	if err != nil {
		return fail(e, err)
	}

	start := time.Now()
	if *dryRunFlag {
		// 演练同样校验 VL 版本能力（R-OPS-04），但不创建快照、不写 S3。
		s := baseSummary(cfg, "backup")
		s.Partitions = formatDays(partitions)
		s.DryRun = true
		s.ElapsedMS = time.Since(start).Milliseconds()
		return finishSummary(e, s, rt.vl.Probe(ctx))
	}

	opts := backup.Options{
		Cluster:           cfg.Cluster,
		Node:              cfg.Node,
		S3Prefix:          effectiveKeyPrefix(cfg),
		Concurrency:       cfg.Concurrency,
		IfExists:          *ifExistsFlag,
		ToolVersion:       currentVersion().Version,
		KeepSnapshot:      *keepSnapshotFlag,
		OrphanSnapshotAge: cfg.OrphanSnapshotAge,
		Logger:            rt.logger,
	}
	var pr *progressReporter
	if !e.jsonOut {
		pr = &progressReporter{w: e.stderr}
		opts.OnProgress = pr.report
	}
	job, err := backup.NewJob(rt.vl, rt.store, rt.locks, opts)
	if err != nil {
		return fail(e, err)
	}

	worst := errs.ExitOK
	for _, p := range partitions {
		if cerr := ctx.Err(); cerr != nil {
			s := baseSummary(cfg, "backup")
			s.Partition = formatDay(p)
			s.ElapsedMS = time.Since(start).Milliseconds()
			code := finishSummary(e, s, errs.Canceled("backup canceled: %v", cerr))
			if code > worst {
				worst = code
			}
			break
		}
		if pr != nil {
			pr.setPartition(p)
		}

		res, runErr := job.Run(ctx, p)

		s := baseSummary(cfg, "backup")
		s.Partition = formatDay(p)
		if len(partitions) > 1 {
			s.Partitions = formatDays(partitions)
		}
		if res != nil {
			s.Files = int64(res.Files)
			s.Bytes = res.Bytes
			s.Skipped = int64(res.Skipped)
			s.Today = res.Today
			s.SnapshotRetained = !res.SnapshotCleaned
			s.ElapsedMS = res.ElapsedMS
			if res.SkippedExisting && runErr == nil {
				// R-BK-06：skip 策略命中文案必须可被调度侧识别。
				fmt.Fprintf(e.stderr, "分区 %s 已有已提交备份，已跳过（--if-exists=replace 可重建）\n", formatDay(p))
			}
		} else {
			// 早期失败（参数/探测/加锁）不返回结果，摘要按计划分区填充。
			s.ElapsedMS = time.Since(start).Milliseconds()
		}
		code := finishSummary(e, s, runErr)
		if code > worst {
			worst = code
		}
	}
	return worst
}
