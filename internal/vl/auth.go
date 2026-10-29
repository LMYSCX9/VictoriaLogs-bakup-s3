// Package vl 是 VictoriaLogs 分区管理 / 快照端点的 HTTP 客户端（访问层）。
// 只认端点与 HTTP 契约，不认备份业务语义；重试只在本层实现（C12，设计文档 §12）。
//
// HTTP 契约（依据 VictoriaLogs 源码 app/vlstorage/main.go 实证，设计文档 §9）：
//
//   - 所有 /internal/* 端点仅接受 POST，否则返回 405；
//   - 参数经查询串传递（服务端以 r.FormValue 读取）；
//   - snapshot/create、snapshot/list、partition/list 返回纯 JSON 字符串数组；
//   - attach/detach 失败返回 500 + 文本错误体，成功返回 200 空体；
//   - snapshot/delete 成功返回 204，失败返回 500 + 文本错误体；
//   - 未匹配的端点返回 404。
package vl

import (
	"net/http"

	"github.com/vlogs-backup/vlogs-backup/internal/config"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// AuthInjector 向每个出站请求注入鉴权头（R-OPS-08，Q1 已决）。
// 实现一律使用标准 Authorization 头，模式与凭据全部来自配置，不硬编码任何业务 header。
type AuthInjector interface {
	Inject(req *http.Request)
}

// NoAuth 不注入鉴权头（vl_auth_mode=none）。
type NoAuth struct{}

func (NoAuth) Inject(*http.Request) {}

// BasicAuth 注入标准 Basic 鉴权头（vl_auth_mode=basic）。
type BasicAuth struct {
	Username string
	Password string
}

func (a BasicAuth) Inject(req *http.Request) { req.SetBasicAuth(a.Username, a.Password) }

// BearerAuth 注入标准 Bearer 鉴权头（vl_auth_mode=bearer）。
type BearerAuth struct {
	Token string
}

func (a BearerAuth) Inject(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+a.Token)
}

// NewAuth 按 vl_auth_mode 构造注入器；凭据缺失或模式未知时返回退出码 2 错误，
// 且不产生任何网络请求（R-OPS-08）。
func NewAuth(mode, username, password, token string) (AuthInjector, error) {
	switch mode {
	case config.AuthModeNone:
		return NoAuth{}, nil
	case config.AuthModeBasic:
		if username == "" || password == "" {
			return nil, errs.Usage("vl_auth_mode basic requires username and password")
		}
		return BasicAuth{Username: username, Password: password}, nil
	case config.AuthModeBearer:
		if token == "" {
			return nil, errs.Usage("vl_auth_mode bearer requires token")
		}
		return BearerAuth{Token: token}, nil
	default:
		return nil, errs.Usage("vl_auth_mode must be one of none/basic/bearer, got %q", mode)
	}
}
