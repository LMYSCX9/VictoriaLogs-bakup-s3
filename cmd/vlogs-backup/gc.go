package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/prune"
)

// runGC 清理 S3 上无 manifest 的未完成前缀（R-GC-01~03）。
// 默认 dry-run；--yes 才真正删除。完整备份单元不在此清理范围（R-GC-04）。
func runGC(e *env, args []string) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	cfgFlag := fs.String("config", "", "配置文件路径（默认 "+defaultConfigPath+"）")
	staleAfterFlag := fs.Duration("stale-after", 24*time.Hour, "无 manifest 前缀的陈旧阈值（如 24h）")
	yesFlag := fs.Bool("yes", false, "执行删除（缺省为 dry-run）")
	dryRunFlag := fs.Bool("dry-run", false, "显式演练模式（与缺省行为一致）")
	jsonFlag := fs.Bool("json", false, "机器可读输出（单行 JSON）")
	if err := fs.Parse(args); err != nil {
		return errs.ExitUsage
	}
	if fs.NArg() > 0 {
		return usageErr(e, "gc: unexpected argument %q", fs.Arg(0))
	}
	e.jsonOut = *jsonFlag
	applyConfigFlag(fs, e, *cfgFlag)

	if *staleAfterFlag <= 0 {
		return usageErr(e, "gc: --stale-after must be positive, got %s", *staleAfterFlag)
	}

	cfg, err := loadConfigFor(context.Background(), e,
		config.FieldS3Endpoint, config.FieldS3Bucket)
	if err != nil {
		return fail(e, err)
	}
	ctx, cancel := commandContext(cfg)
	defer cancel()

	rt, err := buildRuntime(ctx, e, cfg, "gc", runtimeNeeds{s3: true})
	if err != nil {
		return fail(e, err)
	}

	start := time.Now()
	job, err := prune.NewJob(rt.store, rt.keys, prune.Options{
		StaleAfter: *staleAfterFlag,
		Logger:     rt.logger,
	})
	if err != nil {
		return fail(e, err)
	}

	res, runErr := job.Run(ctx, *yesFlag && !*dryRunFlag)

	s := baseSummary(cfg, "gc")
	s.DryRun = res.DryRun
	if res.DryRun {
		s.Units = int64(len(res.Plan.Units))
		s.Objects = int64(res.Plan.TotalObjects)
		s.Bytes = res.Plan.TotalBytes
		s.Partitions = make([]string, len(res.Plan.Units))
		for i, u := range res.Plan.Units {
			s.Partitions[i] = formatDay(u.Partition)
			fmt.Fprintf(e.stderr, "待清理 %s  前缀 %s  %d 对象  %s  最后修改 %s\n",
				formatDay(u.Partition), u.Prefix, u.Objects, humanBytes(u.Bytes),
				u.LastModified.UTC().Format(time.RFC3339))
		}
	} else {
		s.Units = int64(len(res.Deleted))
		s.Objects = int64(res.Objects)
		s.Bytes = res.Bytes
		s.Leftovers = int64(res.Plan.TotalObjects - res.Objects)
		s.Partitions = make([]string, len(res.Deleted))
		for i, u := range res.Deleted {
			s.Partitions[i] = formatDay(u.Partition)
		}
	}
	s.ElapsedMS = time.Since(start).Milliseconds()
	return finishSummary(e, s, runErr)
}
