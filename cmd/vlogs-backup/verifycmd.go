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

// runVerify 校验备份单元完整性（R-MF-02/03）：
// manifest 存在性 + 逐对象 HEAD 比对 size，--deep 时再全量比对 sha256；
// 任一对象缺失或大小不一致以退出码 5 结束，差异明细写 stderr。
func runVerify(e *env, args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	cfgFlag := fs.String("config", "", "配置文件路径（默认 "+defaultConfigPath+"）")
	dateFlag := fs.String("date", "", "校验单日分区，YYYY-MM-DD（默认校验全部已提交备份）")
	deepFlag := fs.Bool("deep", false, "下载全量对象并比对 sha256")
	jsonFlag := fs.Bool("json", false, "机器可读输出（单行 JSON）")
	if err := fs.Parse(args); err != nil {
		return errs.ExitUsage
	}
	if fs.NArg() > 0 {
		return usageErr(e, "verify: unexpected argument %q", fs.Arg(0))
	}
	e.jsonOut = *jsonFlag
	applyConfigFlag(fs, e, *cfgFlag)

	var single string
	if *dateFlag != "" {
		p, err := parseDay("date", *dateFlag)
		if err != nil {
			return usageErr(e, "%v", err)
		}
		single = p
	}

	cfg, err := loadConfigFor(context.Background(), e,
		config.FieldS3Endpoint, config.FieldS3Bucket)
	if err != nil {
		return fail(e, err)
	}
	ctx, cancel := commandContext(cfg)
	defer cancel()

	rt, err := buildRuntime(ctx, e, cfg, "verify", runtimeNeeds{s3: true})
	if err != nil {
		return fail(e, err)
	}

	start := time.Now()
	job, err := verify.NewJob(rt.store, rt.keys, verify.Options{
		Concurrency: cfg.Concurrency,
		Deep:        *deepFlag,
		Logger:      rt.logger,
	})
	if err != nil {
		return fail(e, err)
	}

	partitions := []string{single}
	if single == "" {
		units, lerr := job.List(ctx)
		if lerr != nil {
			s := baseSummary(cfg, "verify")
			s.ElapsedMS = time.Since(start).Milliseconds()
			return finishSummary(e, s, lerr)
		}
		partitions = make([]string, len(units))
		for i, u := range units {
			partitions[i] = u.Partition
		}
	}

	s := baseSummary(cfg, "verify")
	s.Partitions = formatDays(partitions)
	var failed int
	for _, p := range partitions {
		if cerr := ctx.Err(); cerr != nil {
			s.ElapsedMS = time.Since(start).Milliseconds()
			return finishSummary(e, s, errs.Canceled("verify canceled: %v", cerr))
		}
		res, runErr := job.Run(ctx, p)
		if runErr != nil {
			s.ElapsedMS = time.Since(start).Milliseconds()
			return finishSummary(e, s, runErr)
		}
		s.Files += int64(res.Files)
		s.Bytes += res.Bytes
		for _, rel := range res.Missing {
			fmt.Fprintf(e.stderr, "partition %s: missing object %s\n", formatDay(p), rel)
		}
		for _, rel := range res.Mismatched {
			fmt.Fprintf(e.stderr, "partition %s: size/sha256 mismatch %s\n", formatDay(p), rel)
		}
		if !res.OK() {
			failed++
		}
	}

	s.ElapsedMS = time.Since(start).Milliseconds()
	if failed > 0 {
		return finishSummary(e, s, errs.Validation(nil, "%d of %d partition(s) failed verification", failed, len(partitions)))
	}
	return finishSummary(e, s, nil)
}
