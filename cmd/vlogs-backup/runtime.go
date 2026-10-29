package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/backup"
	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/lock"
	"github.com/vlogs-backup/vlogs-backup/internal/purge"
	"github.com/vlogs-backup/vlogs-backup/internal/report"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
	"github.com/vlogs-backup/vlogs-backup/internal/vl"
)

// defaultConfigPath 是配置文件默认位置（设计文档附录 A）。
const defaultConfigPath = "/etc/vlogs-backup/config.toml"

// env 汇总一次子命令执行所需的输入输出与全局选项。
type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	cfgPath     string // 生效的配置文件路径；空表示跳过文件层
	cfgExplicit bool   // 用户显式指定了 --config
	jsonOut     bool   // --json：stdout 只输出单行摘要

	in *bufio.Reader // 懒初始化的 stdin 缓冲
}

// readLine 读取一行交互输入并去除首尾空白（兼容 Windows CRLF）；
// 首行即 EOF 时返回错误，调用方据此按取消处理。
func (e *env) readLine() (string, error) {
	if e.in == nil {
		e.in = bufio.NewReader(e.stdin)
	}
	line, err := e.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// stdinIsTTY 报告 stdin 是否为交互终端（C10：非交互环境必须 --yes）。
func stdinIsTTY(e *env) bool {
	f, ok := e.stdin.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// confirmPartition 要求用户输入完整日期串（YYYY-MM-DD）才继续（R-DEL-03：
// 禁止使用 y/n，输入空行、y 或错误日期一律取消）。
func confirmPartition(e *env, partition string) error {
	day := formatDay(partition)
	fmt.Fprintf(e.stderr,
		"确认操作分区 %s (UTC)：请输入完整日期串 %s 继续，其他输入取消 > ", day, day)
	line, err := e.readLine()
	if err != nil && line == "" {
		return errs.Canceled("partition %s operation canceled: cannot read confirmation: %v", day, err)
	}
	if line != day {
		return errs.Canceled("partition %s operation canceled: confirmation input did not match %s", day, day)
	}
	return nil
}

// confirmYes 是恢复（非今天分区）的交互确认：输入 yes 才继续。
func confirmYes(e *env, partition string) error {
	day := formatDay(partition)
	fmt.Fprintf(e.stderr,
		"确认恢复分区 %s (UTC) 到本地 partitions/：请输入 yes 继续，其他输入取消 > ", day)
	line, err := e.readLine()
	if err != nil && line == "" {
		return errs.Canceled("restore of partition %s canceled: cannot read confirmation: %v", day, err)
	}
	if line != "yes" {
		return errs.Canceled("restore of partition %s canceled: confirmation input was not yes", day)
	}
	return nil
}

// applyConfigFlag 在 flag 解析之后判定 --config 是否被显式设置：
// 仅显式设置时才覆盖 prescan 结果，避免「未传」与「显式传默认值」混淆。
func applyConfigFlag(fs *flag.FlagSet, e *env, path string) {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			set = true
		}
	})
	if !set {
		return
	}
	e.cfgPath = path
	e.cfgExplicit = true
}

// loadConfigFor 装配配置并检查本命令的必填字段。
// 未显式指定 --config 且默认路径不存在时跳过文件层（仅默认值 + 环境变量）。
func loadConfigFor(ctx context.Context, e *env, fields ...string) (*config.Config, error) {
	path := e.cfgPath
	if e.cfgExplicit {
		if path == "" {
			return nil, errs.Usage("--config requires a path argument")
		}
	} else {
		if _, err := os.Stat(defaultConfigPath); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, errs.Usagef(err, "stat config file %s", defaultConfigPath)
			}
			path = ""
		} else {
			path = defaultConfigPath
		}
	}
	cfg, err := config.Load(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Require(fields...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// runtimeNeeds 声明子命令需要的运行时组件。
type runtimeNeeds struct {
	vl      bool // VL 客户端
	s3      bool // S3 客户端
	locks   bool // 本地文件锁
	cleanup bool // 启动期清理 .deleting-* 残留（R-DEL-11，仅变更性命令开启）
}

// kit 是一次子命令执行持有的组件集合。
type kit struct {
	cfg    *config.Config
	logger *slog.Logger
	vl     *vl.Client
	store  *s3store.Store
	locks  *lock.Manager
	keys   *s3store.KeyBuilder
}

// buildRuntime 按需构造组件；日志一律写 stderr，保证 --json 的 stdout 干净。
func buildRuntime(ctx context.Context, e *env, cfg *config.Config, op string, needs runtimeNeeds) (*kit, error) {
	logger := newLogger(e.stderr, op, cfg)
	rt := &kit{cfg: cfg, logger: logger}

	if needs.locks {
		if needs.cleanup {
			n, err := purge.CleanupDeleting(cfg.DataPath, logger)
			if err != nil {
				logger.WarnContext(ctx, "clean leftover .deleting-* dirs failed", "err", err)
			} else if n > 0 {
				logger.WarnContext(ctx, "removed leftover .deleting-* dirs from an interrupted delete", "count", n)
			}
		}
		m, err := lock.New(cfg.DataPath, cfg.Cluster, cfg.Node)
		if err != nil {
			return nil, err
		}
		rt.locks = m
	}

	if needs.vl {
		auth, err := vl.NewAuth(cfg.VLAuthMode, cfg.VLUsername, cfg.VLPassword, cfg.VLToken)
		if err != nil {
			return nil, err
		}
		c, err := vl.NewClient(cfg.VLURL, auth, vl.WithLogger(logger))
		if err != nil {
			return nil, err
		}
		rt.vl = c
	}

	if needs.s3 {
		s, err := s3store.NewStore(s3store.Options{
			Endpoint:           cfg.S3Endpoint,
			Region:             cfg.S3Region,
			Bucket:             cfg.S3Bucket,
			AccessKey:          cfg.S3AccessKey,
			SecretKey:          cfg.S3SecretKey,
			PathStyle:          cfg.S3PathStyle,
			MultipartThreshold: cfg.S3MultipartThreshold,
			MultipartPartSize:  cfg.S3MultipartPartSize,
			Timeout:            cfg.IOTimeout,
			Logger:             logger,
		})
		if err != nil {
			return nil, err
		}
		rt.store = s
	}

	keys, err := s3store.NewKeyBuilder(cfg.S3Prefix, cfg.Cluster, cfg.Node)
	if err != nil {
		return nil, err
	}
	rt.keys = keys
	return rt, nil
}

// newLogger 构造写向 stderr 的结构化日志器：时间键 ts（RFC3339）、错误键 err，
// 并绑定 op/cluster/node；凭据绝不进入日志（C9）。
func newLogger(w io.Writer, op string, cfg *config.Config) *slog.Logger {
	h := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				return slog.Attr{Key: "ts", Value: slog.StringValue(a.Value.Time().UTC().Format(time.RFC3339))}
			case "error":
				return slog.Attr{Key: "err", Value: a.Value}
			default:
				return a
			}
		},
	})
	return slog.New(h).With(slog.String("op", op), slog.String("cluster", cfg.Cluster), slog.String("node", cfg.Node))
}

// commandContext 组合 SIGINT/SIGTERM 与 job_timeout（0 表示不限整体时长）。
func commandContext(cfg *config.Config) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if cfg.JobTimeout > 0 {
		inner, cancel := context.WithTimeout(ctx, cfg.JobTimeout)
		return inner, func() {
			cancel()
			stop()
		}
	}
	return ctx, stop
}

// parseDay 把日期参数归一为 YYYYMMDD（C8：一律按 UTC 解释），
// 同时接受 YYYY-MM-DD 与 YYYYMMDD 两种书写。
func parseDay(flagName, s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errs.Usage("--%s requires a date", flagName)
	}
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.Format("20060102"), nil
		}
	}
	return "", errs.Usage("--%s %q must be a UTC date in YYYY-MM-DD", flagName, s)
}

// formatDay 把 YYYYMMDD 渲染为 YYYY-MM-DD。
func formatDay(partition string) string {
	t, err := time.Parse("20060102", partition)
	if err != nil {
		return partition
	}
	return t.Format("2006-01-02")
}

// formatDays 批量渲染分区日期。
func formatDays(partitions []string) []string {
	out := make([]string, len(partitions))
	for i, p := range partitions {
		out[i] = formatDay(p)
	}
	return out
}

// yesterdayUTC 返回 UTC 昨天（YYYYMMDD）：--date 的默认值。
func yesterdayUTC() string {
	return time.Now().UTC().AddDate(0, 0, -1).Format("20060102")
}

// utcToday 返回 UTC 今天（YYYYMMDD）：当天分区判断一律按 UTC（C8）。
func utcToday() string {
	return time.Now().UTC().Format("20060102")
}

// resolveDates 展开日期参数为升序 YYYYMMDD 列表：
// latest>0 表示最近 N 天（仅 backup），否则 from/to 成对或单日 --date（默认昨天）。
func resolveDates(date, from, to string, latest int, now time.Time) ([]string, error) {
	if latest > 0 {
		if date != "" {
			return nil, errs.Usage("--date cannot be combined with --latest")
		}
		ps, err := backup.ExpandDates("", "", latest, now)
		if err != nil {
			return nil, err
		}
		return ps, nil
	}
	if from != "" || to != "" {
		if date != "" {
			return nil, errs.Usage("--date cannot be combined with --from/--to")
		}
		var pf, pt string
		var err error
		if from != "" {
			if pf, err = parseDay("from", from); err != nil {
				return nil, err
			}
		}
		if to != "" {
			if pt, err = parseDay("to", to); err != nil {
				return nil, err
			}
		}
		return backup.ExpandDates(pf, pt, 0, now)
	}
	d := strings.TrimSpace(date)
	if d == "" {
		d = yesterdayUTC()
	}
	p, err := parseDay("date", d)
	if err != nil {
		return nil, err
	}
	return []string{p}, nil
}

// usageErr 打印参数错误并返回退出码 2（flag 阶段无摘要可渲染）。
func usageErr(e *env, format string, args ...any) int {
	fmt.Fprintf(e.stderr, "vlogs-backup: "+format+"\n", args...)
	return errs.ExitUsage
}

// fail 打印错误并按分类返回退出码。
func fail(e *env, err error) int {
	fmt.Fprintf(e.stderr, "vlogs-backup: %v\n", err)
	return errs.ExitCode(err)
}

// baseSummary 填充通用摘要字段。
func baseSummary(cfg *config.Config, op string) report.Summary {
	return report.Summary{Op: op, Cluster: cfg.Cluster, Node: cfg.Node}
}

// finishSummary 按运行结果补齐 status/error_code 并渲染摘要：
// --json 时 stdout 只有一行 JSON；否则渲染 key: value 表。返回退出码。
func finishSummary(e *env, s report.Summary, runErr error) int {
	s.ErrorCode = errs.ExitCode(runErr)
	switch {
	case runErr != nil:
		if s.ErrorCode == errs.ExitCanceled {
			s.Status = "canceled"
		} else {
			s.Status = "error"
		}
		fmt.Fprintf(e.stderr, "vlogs-backup: %v\n", runErr)
	default:
		s.Status = "success"
	}

	var err error
	if e.jsonOut {
		err = report.JSON(e.stdout, s)
	} else {
		err = report.Table(e.stdout, s)
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "vlogs-backup: %v\n", err)
		return errs.ExitGeneral
	}
	return s.ErrorCode
}

// maxCode 返回 cur 与 err 分类码中较大者（批量作业取最严重退出码）。
func maxCode(cur int, err error) int {
	if c := errs.ExitCode(err); c > cur {
		return c
	}
	return cur
}

// progressReporter 把备份进度节流输出到 stderr（R-BK-08：进度不属于 stdout 摘要）。
type progressReporter struct {
	w io.Writer

	mu        sync.Mutex
	partition string
	last      time.Time
}

// setPartition 切换到下一个分区并重置节流计时。
func (p *progressReporter) setPartition(partition string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partition = partition
	p.last = time.Time{}
}

// report 实现 backup.Options.OnProgress；并发调用安全，最多每 2 秒输出一行。
func (p *progressReporter) report(doneFiles, totalFiles int, doneBytes, totalBytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if doneFiles < totalFiles && !p.last.IsZero() && now.Sub(p.last) < 2*time.Second {
		return
	}
	p.last = now
	fmt.Fprintf(p.w, "进度 %s: %d/%d 文件, %s/%s\n",
		formatDay(p.partition), doneFiles, totalFiles, humanBytes(doneBytes), humanBytes(totalBytes))
}

// humanBytes 人性化字节（二进制单位）。
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
