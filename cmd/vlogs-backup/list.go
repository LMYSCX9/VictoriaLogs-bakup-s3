package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/verify"
)

// runList 列出本节点已提交的备份单元（R-MF-01）：
// 只读扫描 S3，不触碰 VL 与本地分区；无 manifest 的前缀不展示（C3）。
func runList(e *env, args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	cfgFlag := fs.String("config", "", "配置文件路径（默认 "+defaultConfigPath+"）")
	fromFlag := fs.String("from", "", "起始日期，YYYY-MM-DD（含端点）")
	toFlag := fs.String("to", "", "结束日期，YYYY-MM-DD（含端点）")
	jsonFlag := fs.Bool("json", false, "机器可读输出（单行 JSON）")
	if err := fs.Parse(args); err != nil {
		return errs.ExitUsage
	}
	if fs.NArg() > 0 {
		return usageErr(e, "list: unexpected argument %q", fs.Arg(0))
	}
	e.jsonOut = *jsonFlag
	applyConfigFlag(fs, e, *cfgFlag)

	var fromDay, toDay string
	if *fromFlag != "" || *toFlag != "" {
		if *fromFlag == "" || *toFlag == "" {
			return usageErr(e, "list: --from and --to must be used together")
		}
		var err error
		if fromDay, err = parseDay("from", *fromFlag); err != nil {
			return usageErr(e, "%v", err)
		}
		if toDay, err = parseDay("to", *toFlag); err != nil {
			return usageErr(e, "%v", err)
		}
		if fromDay > toDay {
			return usageErr(e, "list: --from %s is after --to %s", formatDay(fromDay), formatDay(toDay))
		}
	}

	cfg, err := loadConfigFor(context.Background(), e,
		config.FieldS3Endpoint, config.FieldS3Bucket)
	if err != nil {
		return fail(e, err)
	}
	ctx, cancel := commandContext(cfg)
	defer cancel()

	rt, err := buildRuntime(ctx, e, cfg, "list", runtimeNeeds{s3: true})
	if err != nil {
		return fail(e, err)
	}

	start := time.Now()
	job, err := verify.NewJob(rt.store, rt.keys, verify.Options{
		Concurrency: cfg.Concurrency,
		Logger:      rt.logger,
	})
	if err != nil {
		return fail(e, err)
	}
	units, lerr := job.List(ctx)
	if lerr != nil {
		s := baseSummary(cfg, "list")
		s.ElapsedMS = time.Since(start).Milliseconds()
		return finishSummary(e, s, lerr)
	}

	// 区间过滤按 YYYYMMDD 字符串比较（升序即日期序）。
	filtered := units[:0]
	for _, u := range units {
		if fromDay != "" && (u.Partition < fromDay || u.Partition > toDay) {
			continue
		}
		filtered = append(filtered, u)
	}

	if e.jsonOut {
		s := baseSummary(cfg, "list")
		s.Partitions = make([]string, len(filtered))
		for i, u := range filtered {
			s.Partitions[i] = formatDay(u.Partition)
			s.Files += int64(u.Files)
			s.Bytes += u.Bytes
		}
		s.ElapsedMS = time.Since(start).Milliseconds()
		return finishSummary(e, s, nil)
	}

	for _, u := range filtered {
		fmt.Fprintf(e.stdout, "%s  %6d files  %s\n", formatDay(u.Partition), u.Files, humanBytes(u.Bytes))
	}
	fmt.Fprintf(e.stdout, "共 %d 个备份单元（cluster=%s node=%s）\n",
		len(filtered), cfg.Cluster, cfg.Node)
	return errs.ExitOK
}
