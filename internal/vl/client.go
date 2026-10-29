package vl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

const (
	// DefaultMaxAttempts 是单次调用的最大尝试次数（含首次；设计文档 §12：VL 5xx 上限 5 次）。
	DefaultMaxAttempts = 5
	// DefaultBackoff 是重试退避的初始时长（设计文档 §12：指数退避 + jitter，初始 500ms）。
	DefaultBackoff = 500 * time.Millisecond

	// maxErrBody 限制错误响应体进错误信息的长度，防止超长文本刷屏。
	maxErrBody = 1024
)

// Client 是 VictoriaLogs 分区管理 / 快照端点的 HTTP 客户端。
// 所有方法线程安全；重试在本层完成，编排层只感知最终分类错误（C12）。
type Client struct {
	baseURL     string
	http        *http.Client
	auth        AuthInjector
	maxAttempts int
	backoff     time.Duration
	logger      *slog.Logger
}

// Option 定制 Client。
type Option func(*Client)

// WithHTTPClient 替换底层 HTTP 客户端（测试注入超时 / 传输层用）。
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithRetry 覆盖重试参数：maxAttempts 为总尝试次数上限（含首次），initialBackoff 为退避初值。
func WithRetry(maxAttempts int, initialBackoff time.Duration) Option {
	return func(c *Client) {
		if maxAttempts >= 1 {
			c.maxAttempts = maxAttempts
		}
		if initialBackoff > 0 {
			c.backoff = initialBackoff
		}
	}
}

// WithLogger 注入结构化 logger；未注入时不打日志。
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.logger = l } }

// NewClient 创建客户端；baseURL 可含路径前缀（vmauth 路由场景），
// 端点会追加在其后。鉴权注入器不可省略，无鉴权时显式传 NoAuth{}。
func NewClient(baseURL string, auth AuthInjector, opts ...Option) (*Client, error) {
	if auth == nil {
		auth = NoAuth{}
	}
	trimmed := strings.TrimRight(baseURL, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, errs.Usagef(err, "parse vl url %q", baseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errs.Usage("vl url %q must use http or https", baseURL)
	}
	if u.Host == "" {
		return nil, errs.Usage("vl url %q must contain a host", baseURL)
	}

	c := &Client{
		baseURL:     trimmed,
		http:        &http.Client{},
		auth:        auth,
		maxAttempts: DefaultMaxAttempts,
		backoff:     DefaultBackoff,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// ForceFlush 触发强制落盘，使内存缓冲写入分区（备份当天分区前建议调用）。
func (c *Client) ForceFlush(ctx context.Context) error {
	_, err := c.post(ctx, "force_flush", "/internal/force_flush", nil)
	return err
}

// SnapshotCreate 为分区前缀创建快照，返回快照绝对路径列表（C2：备份数据唯一合法来源）。
// 支持完整日期（YYYYMMDD）与月份/年前缀。
// 同时发送 partition_prefix 与 name（设计文档 §9）：VL ≥ v1.44.0 按 partition_prefix
// 匹配并忽略 name；v1.31~v1.43 只认已废弃的 name 参数。双参数保证两个版本区间均可创建。
func (c *Client) SnapshotCreate(ctx context.Context, partitionPrefix string) ([]string, error) {
	body, err := c.post(ctx, "snapshot.create", "/internal/partition/snapshot/create",
		url.Values{"partition_prefix": {partitionPrefix}, "name": {partitionPrefix}})
	if err != nil {
		return nil, err
	}
	return parseStringArray(body)
}

// SnapshotDelete 删除一个快照；path 必须是 SnapshotCreate/SnapshotList 返回的实际路径，
// 不做前缀猜测（C6：快照必须清理）。
func (c *Client) SnapshotDelete(ctx context.Context, path string) error {
	_, err := c.post(ctx, "snapshot.delete", "/internal/partition/snapshot/delete",
		url.Values{"path": {path}})
	return err
}

// SnapshotList 返回当前所有分区快照路径（孤立快照清理与能力探测用）。
func (c *Client) SnapshotList(ctx context.Context) ([]string, error) {
	body, err := c.post(ctx, "snapshot.list", "/internal/partition/snapshot/list", nil)
	if err != nil {
		return nil, err
	}
	return parseStringArray(body)
}

// PartitionList 返回当前分区名列表（分区存在性检查与能力探测用）。
func (c *Client) PartitionList(ctx context.Context) ([]string, error) {
	body, err := c.post(ctx, "partition.list", "/internal/partition/list", nil)
	if err != nil {
		return nil, err
	}
	return parseStringArray(body)
}

// Detach 摘除分区，阻塞至在读查询结束（恢复/删除前调用）。
func (c *Client) Detach(ctx context.Context, partition string) error {
	_, err := c.post(ctx, "partition.detach", "/internal/partition/detach",
		url.Values{"name": {partition}})
	return err
}

// Attach 挂载分区（恢复/删除失败回退与恢复末尾调用）。
func (c *Client) Attach(ctx context.Context, partition string) error {
	_, err := c.post(ctx, "partition.attach", "/internal/partition/attach",
		url.Values{"name": {partition}})
	return err
}

// Probe 在作业开始前探测快照与分区管理端点可用性（R-OPS-04）。
// 任一端点 404 视为 VL 版本过低：返回退出码 6 错误，作业不得进入部分执行。
func (c *Client) Probe(ctx context.Context) error {
	probes := []struct {
		name string
		fn   func(context.Context) ([]string, error)
	}{
		{"snapshot/list", c.SnapshotList},
		{"partition/list", c.PartitionList},
	}
	for _, p := range probes {
		if _, err := p.fn(ctx); err != nil {
			var e *errs.Error
			if errors.As(err, &e) && e.Code == errs.ExitNotFound {
				return errs.Precondition(err, "vl version too old: %s endpoint not found", p.name)
			}
			return err
		}
	}
	return nil
}

// versionRe 从 /metrics 的 vm_app_version 行抓取版本号。
var versionRe = regexp.MustCompile(`vm_app_version\{[^}]*version="([^"]*)"`)

// metricsScanLimit 限制 /metrics 的扫描字节数，防止超大响应拖慢调用方。
const metricsScanLimit = 8 << 20

// AppVersion 尽力获取 VL 版本号（GET /metrics 的 vm_app_version 行），
// 仅供摘要展示。/metrics 是监控导出端点而非分区管理端点，GET 为其标准用法；
// 任何失败返回空串（编排层落 unknown），不影响作业主流程。
func (c *Client) AppVersion(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/metrics", nil)
	if err != nil {
		return ""
	}
	c.auth.Inject(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, metricsScanLimit))
	for sc.Scan() {
		if m := versionRe.FindStringSubmatch(sc.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}

// smokeLimit 限制冒烟查询的读取字节数：只需判断响应是否非空。
const smokeLimit = 64 << 10

// SmokeQuery 对分区所在 UTC 天执行最小查询（§7.2 attach 后的冒烟步骤），
// 返回该分区内是否存在任何日志行；只读取响应前 64KB 即可判定非空。
func (c *Client) SmokeQuery(ctx context.Context, partition string) (bool, error) {
	start, err := partitionStart(partition)
	if err != nil {
		return false, err
	}
	query := url.Values{
		"query": {"*"},
		"start": {start.Format(time.RFC3339)},
		"end":   {start.Add(24 * time.Hour).Format(time.RFC3339)},
	}
	target := c.baseURL + "/select/logsql/query?" + query.Encode()

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, errs.Canceled("smoke query: canceled before attempt %d: %v", attempt, err)
		}
		ok, opErr := c.smokeOnce(ctx, target)
		if opErr == nil {
			return ok, nil
		}
		if !opErr.retry || attempt >= c.maxAttempts {
			return false, classify("smoke query", opErr)
		}
		delay := backoffDelay(c.backoff, attempt)
		c.log().WarnContext(ctx, "vl smoke query failed, retrying",
			"op", "smoke.query", "attempt", attempt, "status", opErr.status,
			"delay_ms", delay.Milliseconds(), "err", opErr.err)
		if err := sleep(ctx, delay); err != nil {
			return false, errs.Canceled("smoke query: canceled while waiting for retry: %v", opErr)
		}
	}
}

// smokeOnce 单次冒烟查询：POST /select/logsql/query，只读取前 smokeLimit 字节。
func (c *Client) smokeOnce(ctx context.Context, target string) (bool, *opError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return false, &opError{err: fmt.Errorf("smoke query: build request: %w", err)}
	}
	c.auth.Inject(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return false, &opError{err: fmt.Errorf("smoke query: do request: %w", err), retry: true}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		opErr := &opError{
			status: resp.StatusCode,
			body:   readSnippet(resp.Body),
			retry:  resp.StatusCode >= 500,
		}
		if opErr.retry {
			opErr.err = fmt.Errorf("smoke query: server error")
		} else {
			opErr.err = fmt.Errorf("smoke query: unexpected status %d", resp.StatusCode)
		}
		return false, opErr
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, smokeLimit))
	if err != nil {
		return false, &opError{status: resp.StatusCode, err: fmt.Errorf("smoke query: read response: %w", err)}
	}
	return n > 0, nil
}

// partitionStart 把 YYYYMMDD 分区名解析为该日 UTC 零点（C8：日期一律按 UTC 解释）。
func partitionStart(partition string) (time.Time, error) {
	t, err := time.ParseInLocation("20060102", partition, time.UTC)
	if err != nil {
		return time.Time{}, errs.Usage("partition %q must be in YYYYMMDD form", partition)
	}
	return t, nil
}

// post 执行一次 POST 调用并按设计文档 §12 重试（网络错误与 5xx 重试，4xx 不重试），
// 返回 2xx 响应体。最终错误已分类：404→退出码 4，网络/5xx→退出码 3，其余 4xx→退出码 1。
func (c *Client) post(ctx context.Context, op, endpoint string, query url.Values) ([]byte, error) {
	target := c.baseURL + endpoint
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, errs.Canceled("%s: canceled before attempt %d: %v", op, attempt, err)
		}

		body, opErr := c.once(ctx, op, target)
		if opErr == nil {
			return body, nil
		}
		if !opErr.retry || attempt >= c.maxAttempts {
			return nil, classify(op, opErr)
		}
		delay := backoffDelay(c.backoff, attempt)
		c.log().WarnContext(ctx, "vl call failed, retrying",
			"op", op, "attempt", attempt, "status", opErr.status,
			"delay_ms", delay.Milliseconds(), "err", opErr.err)
		if err := sleep(ctx, delay); err != nil {
			return nil, errs.Canceled("%s: canceled while waiting for retry: %v", op, err)
		}
	}
}

// once 执行单次请求；失败时返回携带分类信息的 opError。
func (c *Client) once(ctx context.Context, op, target string) ([]byte, *opError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return nil, &opError{err: fmt.Errorf("%s: build request: %w", op, err)}
	}
	c.auth.Inject(req)

	resp, err := c.http.Do(req)
	if err != nil {
		// 网络超时 / 连接重置等传输层错误，可重试（§12）。
		return nil, &opError{err: fmt.Errorf("%s: do request: %w", op, err), retry: true}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, &opError{status: resp.StatusCode, err: fmt.Errorf("%s: read response: %w", op, err)}
		}
		return body, nil
	}

	opErr := &opError{
		status: resp.StatusCode,
		body:   readSnippet(resp.Body),
		retry:  resp.StatusCode >= 500,
	}
	if opErr.retry {
		opErr.err = fmt.Errorf("%s: server error", op)
	} else {
		opErr.err = fmt.Errorf("%s: unexpected status %d", op, resp.StatusCode)
	}
	return nil, opErr
}

// opError 是访问层内部的单次失败记录，由 classify 映射为对外分类错误。
type opError struct {
	status int    // 0 表示网络层错误
	body   string // 非 2xx 响应体片段（VL 500 错误体含 attach/detach 失败原因）
	retry  bool
	err    error
}

func (e *opError) Error() string {
	if e.status == 0 {
		return e.err.Error()
	}
	if e.body != "" {
		return e.body
	}
	return fmt.Sprintf("status %d", e.status)
}

func (e *opError) Unwrap() error { return e.err }

// classify 把单次失败映射为对外分类错误（§11.2）：
// 404 → 退出码 4（能力探测据此判定 VL 版本过低）；5xx 与网络错误 → 退出码 3；
// 其余 4xx → 退出码 1（契约异常，需人工介入）。
func classify(op string, e *opError) error {
	switch {
	case e.status == http.StatusNotFound:
		return errs.NotFound(e, "%s: endpoint not found", op)
	case e.status >= 500:
		return errs.Network(e, "%s: vl server returned %d", op, e.status)
	case e.status == 0:
		return errs.Network(e, "%s: request failed", op)
	default:
		return errs.Untyped(e, "%s: unexpected status %d", op, e.status)
	}
}

// parseStringArray 解析 VL 返回的纯 JSON 字符串数组
// （snapshot/create、snapshot/list、partition/list 的响应形态）。
func parseStringArray(body []byte) ([]string, error) {
	var out []string
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errs.Untyped(err, "want a json string array")
	}
	return out, nil
}

// readSnippet 读取错误响应体片段，用于 attach/detach 等失败的定位。
func readSnippet(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, maxErrBody))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// backoffDelay 计算第 attempt 次失败后的退避时长：按指数增长，并在 [d/2, d) 内抖动。
func backoffDelay(initial time.Duration, attempt int) time.Duration {
	d := initial
	for i := 1; i < attempt && d > 0; i++ {
		d *= 2
	}
	if d <= 0 {
		return time.Hour // 溢出保护
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)))
}

// sleep 等待 d 或 ctx 取消，以先到者为准。
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) log() *slog.Logger {
	if c.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.logger
}
