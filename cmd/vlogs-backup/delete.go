package main

import (
	"context"
	"flag"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/purge"
)

// runDelete 删除本地分区数据（R-DEL-01~11，C13）：
// 确认提示 → 逐天护栏 → detach → rename → 递归删除；不得改动 S3 任何对象。
func runDelete(e *env, args []string) int {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	cfgFlag := fs.String("config", "", "配置文件路径（默认 "+defaultConfigPath+"）")
	dateFlag := fs.String("date", "", "删除单日分区，YYYY-MM-DD（默认 UTC 昨天）")
	fromFlag := fs.String("from", "", "批量起始日期，YYYY-MM-DD（含端点）")
	toFlag := fs.String("to", "", "批量结束日期，YYYY-MM-DD（含端点）")
	dryRunFlag := fs.Bool("dry-run", false, "只输出确认提示与规模统计，不执行删除")
	yesFlag := fs.Bool("yes", false, "跳过交互确认（非交互环境必须显式给出）")
	forceNoBackupFlag := fs.Bool("force-no-backup", false, "允许删除没有已提交备份的分区（数据将无法找回）")
	forceTodayFlag := fs.Bool("force-today", false, "允许删除 UTC 今天分区（造成实时日志缺口）")
	jsonFlag := fs.Bool("json", false, "机器可读输出（单行 JSON）")
	if err := fs.Parse(args); err != nil {
		return errs.ExitUsage
	}
	if fs.NArg() > 0 {
		return usageErr(e, "delete: unexpected argument %q", fs.Arg(0))
	}
	e.jsonOut = *jsonFlag
	applyConfigFlag(fs, e, *cfgFlag)

	// C10 / R-DEL-04：非交互环境必须显式 --yes；--dry-run 不进入确认流程。
	if !*dryRunFlag && !stdinIsTTY(e) && !*yesFlag {
		return usageErr(e,
			"delete: refuses to run without --yes in a non-interactive environment (use --dry-run to preview)")
	}

	partitions, err := resolveDates(*dateFlag, *fromFlag, *toFlag, 0, time.Now().UTC())
	if err != nil {
		return usageErr(e, "%v", err)
	}

	cfg, err := loadConfigFor(context.Background(), e,
		config.FieldValuePath, config.FieldDataPath, config.FieldS3Endpoint, config.FieldS3Bucket)
	if err != nil {
		return fail(e, err)
	}
	ctx, cancel := commandContext(cfg)
	defer cancel()

	rt, err := buildRuntime(ctx, e, cfg, "delete", runtimeNeeds{
		vl:      true,
		s3:      true,         // 只读 Head 判断备份护栏（R-DEL-07），禁止任何 S3 写
		locks:   true,         // Inspect 需要 Job；dry-run 只是创建锁目录，不改动分区
		cleanup: !*dryRunFlag, // R-DEL-11：启动清理 .deleting-* 残留
	})
	if err != nil {
		return fail(e, err)
	}

	job, err := purge.NewJob(rt.vl, rt.store, rt.locks, purge.Options{
		Cluster:       cfg.Cluster,
		Node:          cfg.Node,
		S3Prefix:      cfg.S3Prefix,
		DataPath:      cfg.DataPath,
		ForceNoBackup: *forceNoBackupFlag,
		ForceToday:    *forceTodayFlag,
		Logger:        rt.logger,
	})
	if err != nil {
		return fail(e, err)
	}

	start := time.Now()
	worst := errs.ExitOK
	for _, p := range partitions {
		if cerr := ctx.Err(); cerr != nil {
			s := baseSummary(cfg, "delete")
			s.Partition = formatDay(p)
			s.ElapsedMS = time.Since(start).Milliseconds()
			code := finishSummary(e, s, errs.Canceled("delete canceled: %v", cerr))
			if code > worst {
				worst = code
			}
			break
		}

		info, ierr := job.Inspect(ctx, p)
		s := baseSummary(cfg, "delete")
		s.Partition = formatDay(p)
		if ierr != nil {
			s.ElapsedMS = time.Since(start).Milliseconds()
			code := finishSummary(e, s, ierr)
			if code > worst {
				worst = code
			}
			continue
		}

		// 护栏先于确认提示：不存在（4）/ 今天分区（6）/ 无备份（6）。
		if gerr := purge.Guard(info, *forceNoBackupFlag, *forceTodayFlag); gerr != nil {
			s.ElapsedMS = time.Since(start).Milliseconds()
			code := finishSummary(e, s, gerr)
			if code > worst {
				worst = code
			}
			continue
		}

		// R-DEL-02：确认提示固定输出到 stderr（保证 --json 的 stdout 不被污染）。
		purge.RenderConfirm(e.stderr, info)
		s.Path = info.Path
		s.Files = int64(info.Files)
		s.Bytes = info.Bytes
		s.HasBackup = info.HasBackup

		if *dryRunFlag {
			s.DryRun = true
			s.ElapsedMS = time.Since(start).Milliseconds()
			finishSummary(e, s, nil)
			continue
		}

		// R-DEL-03：交互终端必须输入完整日期串；非交互路径已由 --yes 前置检查放行。
		if stdinIsTTY(e) {
			if cerr := confirmPartition(e, p); cerr != nil {
				s.ElapsedMS = time.Since(start).Milliseconds()
				code := finishSummary(e, s, cerr)
				if code > worst {
					worst = code
				}
				continue
			}
		}

		res, runErr := job.Run(ctx, p)
		if runErr == nil {
			s.Path = res.Path
			s.Files = int64(res.Files)
			s.Bytes = res.Bytes
			s.ElapsedMS = res.ElapsedMS
		} else {
			s.ElapsedMS = time.Since(start).Milliseconds()
		}
		code := finishSummary(e, s, runErr)
		if code > worst {
			worst = code
		}
	}
	return worst
}
