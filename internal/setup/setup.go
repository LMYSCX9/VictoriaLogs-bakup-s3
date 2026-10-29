// Package setup 是交互式初始化向导（设计文档 §10.3）：按固定顺序逐项询问
// 并生成 config.toml（0644）与 credentials（0600）；凭据经 Prompter 注入，
// 不回显、不打印、不落日志（C9，R-SU-01~07）。
package setup

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
	"github.com/vlogs-backup/vlogs-backup/internal/vl"
)

// DefaultOutputDir 是配置生成目录默认值（--output-dir 可覆盖）。
const DefaultOutputDir = "/etc/vlogs-backup"

// probeTimeout 是单次连通性探测的上限：交互场景快速失败，由用户决定重试/重输/中止。
const probeTimeout = 10 * time.Second

// Answers 收集向导的全部应答；带「敏感」注释的字段禁止进入任何输出（C9）。
type Answers struct {
	VLURL       string
	AuthMode    string
	VLUsername  string
	VLPassword  string // 敏感
	VLToken     string // 敏感
	DataPath    string
	Cluster     string
	Node        string
	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3Prefix    string
	S3PathStyle bool
	S3AccessKey string // 敏感
	S3SecretKey string // 敏感
}

// Options 是向导的运行参数。
type Options struct {
	OutputDir string // 空 → DefaultOutputDir
	SkipCheck bool   // 跳过连通性验证（R-SU-04）
	Stdout    io.Writer
	Stderr    io.Writer
}

// Wizard 是交互式初始化向导（设计文档 §8.2）。
// checkVL / checkS3 为故障注入点（nil 时走真实探测），仅限包内测试替换。
type Wizard struct {
	prompter Prompter
	opts     Options

	checkVL func(ctx context.Context, a *Answers) error
	checkS3 func(ctx context.Context, a *Answers) error
}

// NewWizard 构造向导。
func NewWizard(opts Options, p Prompter) *Wizard {
	if opts.OutputDir == "" {
		opts.OutputDir = DefaultOutputDir
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	return &Wizard{prompter: p, opts: opts}
}

// Run 执行完整向导：TTY 检查 → 覆盖确认 → 固定顺序询问 → 连通性验证 →
// 落盘 → 摘要。验证未通过前不落盘任何文件（R-SU-04）。
func (w *Wizard) Run(ctx context.Context) error {
	if !w.prompter.Interactive() {
		return errs.Usage("setup requires an interactive terminal; use flags or VLB_* environment variables instead")
	}

	cfgPath := filepath.Join(w.opts.OutputDir, "config.toml")
	if fileExists(cfgPath) {
		fmt.Fprintf(w.opts.Stdout, "目标配置文件已存在: %s\n", cfgPath)
		ans, err := w.prompter.Line("输入 overwrite 确认覆盖，其他输入取消", "")
		if err != nil {
			return errs.Canceled("setup canceled while confirming overwrite: %v", err)
		}
		if strings.TrimSpace(ans) != "overwrite" {
			return errs.Canceled("setup canceled: existing config not overwritten")
		}
	}

	a := &Answers{}
	fmt.Fprintln(w.opts.Stdout, "开始逐项配置：每项前的「·」说明解释该配置的用途；输错可退格修正（^H / ^? 均有效），Ctrl-C 随时中止。")
	for {
		if err := w.ask(a); err != nil {
			return err
		}
		if w.opts.SkipCheck {
			break
		}
		reenter, err := w.verify(ctx, a)
		if err != nil {
			return err
		}
		if !reenter {
			break
		}
	}

	if err := writeFiles(w.opts.OutputDir, a); err != nil {
		return err
	}
	w.printSummary(a)
	return nil
}

// verify 执行连通性验证；返回 reenter=true 表示用户选择重新输入。
// 中止返回退出码 3（R-SU-04）。
func (w *Wizard) verify(ctx context.Context, a *Answers) (reenter bool, err error) {
probe:
	for {
		if perr := w.probeAll(ctx, a); perr != nil {
			fmt.Fprintf(w.opts.Stderr, "连通性验证失败: %v\n", perr)
			for {
				ans, lerr := w.prompter.Line("选择操作: [r] 重试 / [v] 重新输入 / [a] 中止", "r")
				if lerr != nil {
					return false, errs.Canceled("setup canceled while choosing verify action: %v", lerr)
				}
				switch strings.TrimSpace(ans) {
				case "", "r", "R":
					continue probe
				case "v", "V":
					return true, nil
				case "a", "A":
					return false, errs.Network(nil, "setup aborted by user after failed connectivity check")
				default:
					fmt.Fprintln(w.opts.Stderr, "无效选择，请输入 r / v / a")
				}
			}
		}
		return false, nil
	}
}

// probeAll 依次探测 VL 与 S3；探测本身不重试（交互场景快速失败）。
func (w *Wizard) probeAll(ctx context.Context, a *Answers) error {
	checkVL, checkS3 := w.checkVL, w.checkS3
	if checkVL == nil {
		checkVL = defaultCheckVL
	}
	if checkS3 == nil {
		checkS3 = defaultCheckS3
	}
	if err := checkVL(ctx, a); err != nil {
		return fmt.Errorf("vl: %w", err)
	}
	if err := checkS3(ctx, a); err != nil {
		return fmt.Errorf("s3: %w", err)
	}
	return nil
}

// defaultCheckVL 走能力探测：快照 list 端点可达（R-SU-04 / 设计文档 §10.3）。
func defaultCheckVL(ctx context.Context, a *Answers) error {
	auth, err := vl.NewAuth(a.AuthMode, a.VLUsername, a.VLPassword, a.VLToken)
	if err != nil {
		return err
	}
	client, err := vl.NewClient(a.VLURL, auth, vl.WithRetry(1, time.Millisecond))
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, err = client.SnapshotList(pctx)
	return err
}

// defaultCheckS3 走 HeadBucket（R-SU-04）。
func defaultCheckS3(ctx context.Context, a *Answers) error {
	store, err := s3store.NewStore(s3store.Options{
		Endpoint:    a.S3Endpoint,
		Region:      a.S3Region,
		Bucket:      a.S3Bucket,
		AccessKey:   a.S3AccessKey,
		SecretKey:   a.S3SecretKey,
		PathStyle:   a.S3PathStyle,
		MaxAttempts: 1,
		Timeout:     probeTimeout,
	})
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return store.HeadBucket(pctx)
}

// hint 输出一行中文说明，解释下一个配置项的用途与取值形式（R-SU-02）。
func (w *Wizard) hint(text string) {
	fmt.Fprintf(w.opts.Stdout, "  · %s\n", text)
}

// ask 按设计文档 §10.3 的固定顺序逐项询问并写入 a；每项前输出一行中文说明（R-SU-02）。
func (w *Wizard) ask(a *Answers) error {
	var err error

	// 1. VL 地址（URL 校验，不合法重输）
	w.hint("VL 服务的 HTTP 地址：直连形如 http://<IP>:9428；经 vmauth 代理时填代理地址（可含路径前缀）")
	if a.VLURL, err = w.askURL("VL 地址", "http://127.0.0.1:9428"); err != nil {
		return err
	}

	// 2. 鉴权模式；basic 追问 username/password、bearer 追问 token（均不回显）
	w.hint("与 VL（或前置 vmauth）约定的鉴权方式：none=无鉴权，basic=用户名+密码，bearer=令牌")
	if a.AuthMode, err = w.askAuthMode(); err != nil {
		return err
	}
	a.VLUsername, a.VLPassword, a.VLToken = "", "", ""
	switch a.AuthMode {
	case config.AuthModeBasic:
		w.hint("basic 鉴权的用户名")
		if a.VLUsername, err = w.askRequired("VL 用户名"); err != nil {
			return err
		}
		w.hint("basic 鉴权的密码，输入不回显")
		if a.VLPassword, err = w.askSecret("VL 密码", true); err != nil {
			return err
		}
	case config.AuthModeBearer:
		w.hint("bearer 鉴权的令牌，输入不回显")
		if a.VLToken, err = w.askSecret("VL Token", true); err != nil {
			return err
		}
	}

	// 3. 数据目录（绝对路径）
	w.hint("VL 的 --storageDataPath 数据目录（绝对路径）：备份直接读其分区快照，必须与 VL 同机")
	if a.DataPath, err = w.askAbsPath("数据目录"); err != nil {
		return err
	}

	// 4. cluster / node
	w.hint("集群标识：写入 S3 键用于多套集群隔离，单集群保持默认即可")
	if a.Cluster, err = w.askLine("cluster", "default"); err != nil {
		return err
	}
	w.hint("节点标识：建议主机名或 IP，同集群多节点不得重复")
	if a.Node, err = w.askLine("node", "default"); err != nil {
		return err
	}

	// 5. S3 endpoint（URL 校验）
	w.hint("S3 兼容存储的服务地址（MinIO / RustFS / 公有云 endpoint）")
	if a.S3Endpoint, err = w.askURL("S3 endpoint", "http://127.0.0.1:9000"); err != nil {
		return err
	}

	// 6. S3 region（可留空，RustFS / MinIO 等）
	w.hint("S3 区域：MinIO / RustFS 等私有部署可留空")
	if a.S3Region, err = w.askLine("S3 region（可留空）", ""); err != nil {
		return err
	}

	// 7. bucket / prefix
	w.hint("备份目标桶：需已创建且账号有读写权限")
	if a.S3Bucket, err = w.askRequired("S3 bucket"); err != nil {
		return err
	}
	w.hint("桶内对象键前缀：多套备份共用一个桶时用于相互隔离")
	if a.S3Prefix, err = w.askLine("S3 prefix", "vlogs"); err != nil {
		return err
	}

	// 8. path-style
	w.hint("寻址风格：true=路径风格 http://ep/bucket/key（MinIO/RustFS 选 true），false=虚拟主机风格（公有云常用）")
	if a.S3PathStyle, err = w.askBool("使用 path-style", true); err != nil {
		return err
	}

	// 9. AK / SK（必填；不回显）
	w.hint("S3 访问密钥：必填，写入 credentials 文件（0600）；留空则 backup/restore 等命令失败（退出码 2）")
	if a.S3AccessKey, err = w.askSecret("S3 Access Key", false); err != nil {
		return err
	}
	w.hint("与 Access Key 配对的秘密密钥，输入不回显")
	if a.S3SecretKey, err = w.askSecret("S3 Secret Key", false); err != nil {
		return err
	}
	return nil
}

// askLine 读一行文本，去除首尾空白后返回（防粘贴带入空白）；空输入回落默认值
// （默认值语义由向导层裁决，便于测试桩注入原始应答）；读取失败视为用户取消
// （退出码 7）。
func (w *Wizard) askLine(label, def string) (string, error) {
	ans, err := w.prompter.Line(label, def)
	if err != nil {
		return "", errs.Canceled("setup canceled while reading %q: %v", label, err)
	}
	ans = strings.TrimSpace(ans)
	if ans == "" {
		ans = def
	}
	return ans, nil
}

// askURL 询问 http(s) URL，非法则要求重输（R-SU-02）。
func (w *Wizard) askURL(label, def string) (string, error) {
	for {
		ans, err := w.askLine(label, def)
		if err != nil {
			return "", err
		}
		if validURL(ans) {
			return ans, nil
		}
		fmt.Fprintf(w.opts.Stderr, "%s 不合法（需要 http:// 或 https:// 的完整 URL），请重输\n", label)
	}
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (w *Wizard) askAuthMode() (string, error) {
	for {
		ans, err := w.askLine("VL 鉴权模式 (none/basic/bearer)", config.AuthModeNone)
		if err != nil {
			return "", err
		}
		switch mode := strings.TrimSpace(ans); mode {
		case config.AuthModeNone, config.AuthModeBasic, config.AuthModeBearer:
			return mode, nil
		default:
			fmt.Fprintln(w.opts.Stderr, "鉴权模式必须是 none / basic / bearer，请重输")
		}
	}
}

func (w *Wizard) askRequired(label string) (string, error) {
	for {
		ans, err := w.askLine(label, "")
		if err != nil {
			return "", err
		}
		if ans = strings.TrimSpace(ans); ans != "" {
			return ans, nil
		}
		fmt.Fprintf(w.opts.Stderr, "%s 不能为空，请重输\n", label)
	}
}

// askSecret 询问一个不回显的秘密值；required 为 false 时空输入直接返回。
func (w *Wizard) askSecret(label string, required bool) (string, error) {
	for {
		ans, err := w.prompter.Password(label)
		if err != nil {
			return "", errs.Canceled("setup canceled while reading %q: %v", label, err)
		}
		if ans != "" || !required {
			return ans, nil
		}
		fmt.Fprintf(w.opts.Stderr, "%s 不能为空，请重输\n", label)
	}
}

// askAbsPath 询问绝对路径（设计文档 §10.3 步骤 3 校验）。
func (w *Wizard) askAbsPath(label string) (string, error) {
	for {
		ans, err := w.askRequired(label)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(ans) {
			return filepath.Clean(ans), nil
		}
		fmt.Fprintf(w.opts.Stderr, "%s 必须是绝对路径，请重输\n", label)
	}
}

func (w *Wizard) askBool(label string, def bool) (bool, error) {
	for {
		ans, err := w.askLine(label+" (true/false)", strconv.FormatBool(def))
		if err != nil {
			return false, err
		}
		if b, perr := strconv.ParseBool(strings.TrimSpace(ans)); perr == nil {
			return b, nil
		}
		fmt.Fprintf(w.opts.Stderr, "%s 需要 true 或 false，请重输\n", label)
	}
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
