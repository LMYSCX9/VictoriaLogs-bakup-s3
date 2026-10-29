package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// fakePrompter 脚本化应答：lines 按 Line/Password 的调用顺序弹出。
type fakePrompter struct {
	lines       []string
	i           int
	interactive bool
	prompts     []string
}

func (f *fakePrompter) Line(prompt, def string) (string, error) {
	f.prompts = append(f.prompts, prompt+" ["+def+"]")
	return f.next()
}

func (f *fakePrompter) Password(prompt string) (string, error) {
	f.prompts = append(f.prompts, prompt)
	return f.next()
}

func (f *fakePrompter) next() (string, error) {
	if f.i >= len(f.lines) {
		return "", io.EOF
	}
	s := f.lines[f.i]
	f.i++
	return s, nil
}

func (f *fakePrompter) Interactive() bool { return f.interactive }

func (f *fakePrompter) countPrompts(prefix string) int {
	n := 0
	for _, p := range f.prompts {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return n
}

// noneScript 是 auth=none 时一套完整应答（12 项）。
func noneScript(datapath, bucket string) []string {
	return []string{
		"",       // VL 地址 → 默认
		"",       // auth mode → none
		datapath, // 数据目录
		"", "",   // cluster / node → default
		"",     // S3 endpoint → 默认
		"",     // region
		bucket, // bucket（必填）
		"",     // prefix → vlogs
		"",     // path-style → true
		"", "", // AK / SK（空输入 → 运行命令时报退出码 2）
	}
}

// vlbKeys 是可能影响 config.Load 结果的环境变量，测试中统一清空。
var vlbKeys = []string{
	"VLB_VL_URL", "VLB_VL_AUTH_MODE", "VLB_VL_USERNAME", "VLB_VL_PASSWORD", "VLB_VL_TOKEN",
	"VLB_DATA_PATH", "VLB_CLUSTER", "VLB_NODE",
	"VLB_S3_ENDPOINT", "VLB_S3_REGION", "VLB_S3_BUCKET", "VLB_S3_PREFIX", "VLB_S3_PATH_STYLE",
	"VLB_S3_ACCESS_KEY", "VLB_S3_SECRET_KEY",
}

func clearVLBEnv(t *testing.T) {
	t.Helper()
	for _, k := range vlbKeys {
		t.Setenv(k, "")
	}
}

func newTestWizard(t *testing.T, lines []string) (*Wizard, *fakePrompter, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	clearVLBEnv(t)
	p := &fakePrompter{lines: lines, interactive: true}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	w := NewWizard(Options{OutputDir: t.TempDir(), Stdout: out, Stderr: errOut}, p)
	// 默认桩掉连通性探测，避免测试触碰 127.0.0.1:9428/9000；需要失败路径的用例自行覆盖。
	w.checkVL = func(context.Context, *Answers) error { return nil }
	w.checkS3 = func(context.Context, *Answers) error { return nil }
	return w, p, out, errOut
}

func assertNoLeak(t *testing.T, secrets []string, outs ...string) {
	t.Helper()
	for _, s := range secrets {
		for _, o := range outs {
			if strings.Contains(o, s) {
				t.Errorf("secret %q leaked into output", s)
			}
		}
	}
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // Windows 的 chmod 只保留只读位，断言无意义
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s perm = %o, want %o", path, got, want)
	}
}

// TestRunDefaults 验收 R-SU-02：空输入全部落到默认值（auth 默认 none 不追问凭据），
// 且 R-SU-07：产物被 config.Load 直接消费；全空凭据时不创建 credentials 文件。
func TestRunDefaults(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	w, p, _, _ := newTestWizard(t, noneScript(datapath, "backups"))

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	cfgPath := filepath.Join(w.opts.OutputDir, "config.toml")
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	if cfg.VLURL != "http://127.0.0.1:9428" {
		t.Errorf("vl_url = %q", cfg.VLURL)
	}
	if cfg.VLAuthMode != config.AuthModeNone {
		t.Errorf("auth mode = %q", cfg.VLAuthMode)
	}
	if cfg.DataPath != datapath || cfg.S3Bucket != "backups" {
		t.Errorf("data_path = %q, bucket = %q", cfg.DataPath, cfg.S3Bucket)
	}
	if cfg.Cluster != "default" || cfg.Node != "default" || cfg.S3Prefix != "vlogs" || !cfg.S3PathStyle {
		t.Errorf("defaults not applied: cluster=%s node=%s prefix=%s pathStyle=%t",
			cfg.Cluster, cfg.Node, cfg.S3Prefix, cfg.S3PathStyle)
	}
	if _, err := os.Stat(filepath.Join(w.opts.OutputDir, "credentials")); !os.IsNotExist(err) {
		t.Errorf("credentials should not exist when no secret given, stat err = %v", err)
	}
	// 默认值展示在提示中
	for _, want := range []string{"[http://127.0.0.1:9428]", "[default]", "[vlogs]", "[true]", "[none]"} {
		if !strings.Contains(strings.Join(p.prompts, "\n"), want) {
			t.Errorf("prompt default %q not shown", want)
		}
	}
	assertPerm(t, cfgPath, 0o644)
}

// TestRunBasicAuth 验收 R-SU-01/03：credentials（0600）承载敏感项，
// config.toml 与全部输出中 secret 零命中，摘要以 *** 代替。
func TestRunBasicAuth(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	const (
		pass = "s3cret-Pass!"
		ak   = "AKIAIOSFODNN7EXAMPLE"
		sk   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCY"
	)
	lines := []string{
		"", "basic", "alice", pass, datapath, "", "", "", "", "backups", "", "", ak, sk,
	}
	w, p, out, errOut := newTestWizard(t, lines)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	cfg, err := config.Load(context.Background(), filepath.Join(w.opts.OutputDir, "config.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.VLUsername != "alice" || cfg.VLPassword != pass {
		t.Errorf("basic credentials not round-tripped: user=%q pass set=%t", cfg.VLUsername, cfg.VLPassword == pass)
	}
	if cfg.S3AccessKey != ak || cfg.S3SecretKey != sk {
		t.Errorf("s3 credentials not round-tripped")
	}

	tomlBytes, _ := os.ReadFile(filepath.Join(w.opts.OutputDir, "config.toml"))
	assertNoLeak(t, []string{pass, ak, sk}, string(tomlBytes), out.String(), errOut.String(), strings.Join(p.prompts, "\n"))
	if !strings.Contains(out.String(), "***") {
		t.Errorf("summary should mask secrets with ***")
	}
	assertPerm(t, filepath.Join(w.opts.OutputDir, "credentials"), 0o600)
}

// TestRunBearerToken 验收 R-SU-01：bearer 模式只写 vl_token。
func TestRunBearerToken(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	const tok = "tok-XYZ-123"
	lines := []string{"", "bearer", tok, datapath, "", "", "", "", "backups", "", "", "", ""}
	w, _, out, errOut := newTestWizard(t, lines)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	cfg, err := config.Load(context.Background(), filepath.Join(w.opts.OutputDir, "config.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.VLToken != tok {
		t.Errorf("token not round-tripped")
	}
	cred, _ := os.ReadFile(filepath.Join(w.opts.OutputDir, "credentials"))
	if !strings.Contains(string(cred), "vl_token") || strings.Contains(string(cred), "s3_access_key") {
		t.Errorf("credentials = %q, want only vl_token", string(cred))
	}
	assertNoLeak(t, []string{tok}, out.String(), errOut.String())
}

// TestHintsShown 验收 R-SU-02：总引导行存在，每项询问前输出中文说明。
func TestHintsShown(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	w, _, out, _ := newTestWizard(t, noneScript(datapath, "backups"))

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	o := out.String()
	if !strings.Contains(o, "开始逐项配置") {
		t.Errorf("intro line missing")
	}
	for _, frag := range []string{
		"VL 服务的 HTTP 地址", "鉴权方式", "--storageDataPath", "集群标识", "节点标识",
		"S3 兼容存储", "S3 区域", "备份目标桶", "对象键前缀", "寻址风格", "S3 访问密钥", "秘密密钥",
	} {
		if !strings.Contains(o, frag) {
			t.Errorf("hint %q missing", frag)
		}
	}
}

// TestAskLineTrims 验收 R-SU-02：粘贴带入首尾空白时被去除。
func TestAskLineTrims(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	lines := append([]string{"  http://10.0.0.8:9428  "}, noneScript(datapath, "backups")[1:]...)
	w, _, _, _ := newTestWizard(t, lines)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	cfg, err := config.Load(context.Background(), filepath.Join(w.opts.OutputDir, "config.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.VLURL != "http://10.0.0.8:9428" {
		t.Errorf("vl_url = %q, want trimmed", cfg.VLURL)
	}
}

// TestURLReask 验收 R-SU-02：非法 URL 被要求重输。
func TestURLReask(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	lines := append([]string{"bogus", "ftp://x", ""}, noneScript(datapath, "backups")[1:]...)
	w, p, _, _ := newTestWizard(t, lines)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := p.countPrompts("VL 地址"); n != 3 {
		t.Errorf("VL 地址 prompted %d times, want 3 (2 invalid + 1 default)", n)
	}
}

// TestDataPathReask 验收 §10.3 步骤 3：相对路径被拒绝。
func TestDataPathReask(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	// 前 2 项走默认，第 3 项给非法相对路径，第 4 项给合法绝对路径，
	// 其余从 noneScript 的 cluster 项开始对齐。
	lines := []string{"", "", "relative/path", datapath}
	lines = append(lines, noneScript(datapath, "backups")[3:]...)
	w, p, _, _ := newTestWizard(t, lines)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := p.countPrompts("数据目录"); n != 2 {
		t.Errorf("数据目录 prompted %d times, want 2", n)
	}
}

// TestRunCheckAbort 验收 R-SU-04：验证失败 → 重试两次后中止 → 退出码 3，无文件落盘。
func TestRunCheckAbort(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	lines := append(noneScript(datapath, "backups"), "r", "r", "a")
	w, _, _, errOut := newTestWizard(t, lines)

	w.checkVL = func(context.Context, *Answers) error { return errors.New("boom") }
	w.checkS3 = func(context.Context, *Answers) error { return nil }

	err := w.Run(context.Background())
	if got := errs.ExitCode(err); got != errs.ExitNetwork {
		t.Fatalf("exit code = %d, want 3; err = %v", got, err)
	}
	if !strings.Contains(errOut.String(), "连通性验证失败") {
		t.Errorf("failure reason not shown")
	}
	if _, serr := os.Stat(filepath.Join(w.opts.OutputDir, "config.toml")); !os.IsNotExist(serr) {
		t.Errorf("no file should be written before check passes")
	}
}

// TestRunCheckRetry 验收 R-SU-04：失败后选择重试，第二次通过。
func TestRunCheckRetry(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	lines := append(noneScript(datapath, "backups"), "r")
	w, _, _, _ := newTestWizard(t, lines)

	calls := 0
	w.checkS3 = func(context.Context, *Answers) error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	}
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 2 {
		t.Errorf("checkS3 calls = %d, want 2", calls)
	}
	if _, err := os.Stat(filepath.Join(w.opts.OutputDir, "config.toml")); err != nil {
		t.Errorf("config.toml should exist: %v", err)
	}
}

// TestRunCheckReenter 验收 R-SU-04：失败后选择重新输入，完整重问一遍。
func TestRunCheckReenter(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	lines := noneScript(datapath, "backups")
	lines = append(lines, "v")
	lines = append(lines, noneScript(datapath, "backups")...)
	w, p, _, _ := newTestWizard(t, lines)

	calls := 0
	w.checkVL = func(context.Context, *Answers) error {
		calls++
		if calls == 1 {
			return errors.New("down")
		}
		return nil
	}
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 2 {
		t.Errorf("checkVL calls = %d, want 2", calls)
	}
	if n := p.countPrompts("VL 地址"); n != 2 {
		t.Errorf("VL 地址 prompted %d times, want 2 after reenter", n)
	}
}

// TestOverwriteDeclined 验收 R-SU-05：输入非 overwrite 时取消（退出码 7），原文件未动。
func TestOverwriteDeclined(t *testing.T) {
	w, p, _, _ := newTestWizard(t, []string{"no"})
	cfgPath := filepath.Join(w.opts.OutputDir, "config.toml")
	os.WriteFile(cfgPath, []byte("old content"), 0o644)

	err := w.Run(context.Background())
	if got := errs.ExitCode(err); got != errs.ExitCanceled {
		t.Fatalf("exit code = %d, want 7; err = %v", got, err)
	}
	b, _ := os.ReadFile(cfgPath)
	if string(b) != "old content" {
		t.Errorf("existing config was modified")
	}
	if len(p.prompts) != 1 {
		t.Errorf("prompts = %v, want only the overwrite confirm", p.prompts)
	}
}

// TestOverwriteAccepted 验收 R-SU-05：输入 overwrite 后正常覆盖。
func TestOverwriteAccepted(t *testing.T) {
	datapath := filepath.Join(t.TempDir(), "vlstorage")
	lines := append([]string{"overwrite"}, noneScript(datapath, "backups")...)
	w, _, _, _ := newTestWizard(t, lines)
	os.WriteFile(filepath.Join(w.opts.OutputDir, "config.toml"), []byte("old content"), 0o644)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(w.opts.OutputDir, "config.toml"))
	if !strings.Contains(string(b), "vlogs") {
		t.Errorf("config was not overwritten: %s", string(b))
	}
}

// TestNonTTYRejected 验收 R-SU-06：非交互终端以退出码 2 拒绝，无任何询问。
func TestNonTTYRejected(t *testing.T) {
	clearVLBEnv(t)
	p := &fakePrompter{interactive: false}
	w := NewWizard(Options{OutputDir: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard}, p)

	err := w.Run(context.Background())
	if got := errs.ExitCode(err); got != errs.ExitUsage {
		t.Fatalf("exit code = %d, want 2; err = %v", got, err)
	}
	if p.i != 0 {
		t.Errorf("no prompt should be consumed in non-interactive mode")
	}
	if _, serr := os.Stat(filepath.Join(w.opts.OutputDir, "config.toml")); !os.IsNotExist(serr) {
		t.Errorf("no file should be written in non-interactive mode")
	}
}

// TestDefaultCheckVL 覆盖默认 VL 探测：httptest 可达返回 nil，不可达返回错误。
func TestDefaultCheckVL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/partition/snapshot/list" {
			fmt.Fprint(w, "[]")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ok := &Answers{VLURL: srv.URL, AuthMode: config.AuthModeNone}
	if err := defaultCheckVL(context.Background(), ok); err != nil {
		t.Errorf("reachable endpoint: %v", err)
	}
	bad := &Answers{VLURL: "http://127.0.0.1:1", AuthMode: config.AuthModeNone}
	if err := defaultCheckVL(context.Background(), bad); err == nil {
		t.Errorf("unreachable endpoint should fail")
	}
}

// TestDefaultCheckS3 覆盖默认 S3 探测（HeadBucket）：httptest 可达返回 nil，不可达返回错误。
func TestDefaultCheckS3(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ok := &Answers{S3Endpoint: srv.URL, S3Bucket: "b", S3PathStyle: true, S3AccessKey: "ak", S3SecretKey: "sk"}
	if err := defaultCheckS3(context.Background(), ok); err != nil {
		t.Errorf("reachable bucket: %v", err)
	}
	bad := &Answers{S3Endpoint: "http://127.0.0.1:1", S3Bucket: "b", S3PathStyle: true, S3AccessKey: "ak", S3SecretKey: "sk"}
	if err := defaultCheckS3(context.Background(), bad); err == nil {
		t.Errorf("unreachable endpoint should fail")
	}
}
