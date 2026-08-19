// login.go 登录管理器：TRAE（本地回调自动捕获）与 WorkBuddy（轮询设备流）。
//
// 安全设计：
//   - TRAE 回调监听仅绑 127.0.0.1，仅登录期间存在（5 分钟超时自动关闭）
//   - 回调页面不回显任何敏感参数
//   - 一次只允许一个登录流程（loginMu 互斥）
package app

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"work2api-desktop/internal/auth"
	"work2api-desktop/internal/upstream/trae"
	"work2api-desktop/internal/upstream/workbuddy"
)

const loginTimeout = 5 * time.Minute

// LoginStatusResponse 前端轮询的登录状态。
type LoginStatusResponse struct {
	Active   bool         `json:"active"`   // 是否有进行中的登录流程
	Provider string       `json:"provider"` // trae | workbuddy
	Status   string       `json:"status"`   // pending | done | error
	Err      string       `json:"err,omitempty"`
	Result   *LoginResult `json:"result,omitempty"`
}

// StartLogin 发起登录，返回浏览器授权 URL。
func (c *Core) StartLogin(provider string) (string, error) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()

	// 已有流程未结束 → 拒绝（防并发登录串会话）
	if c.login != nil && c.login.status == "pending" {
		return "", fmt.Errorf("a login flow is already in progress")
	}
	// 清理旧监听
	if c.login != nil && c.login.listener != nil {
		_ = c.login.listener.Close()
	}
	c.login = nil

	switch provider {
	case auth.ProviderTrae:
		return c.startTraeLogin()
	case auth.ProviderWorkBuddy:
		return c.startWorkBuddyLogin()
	default:
		return "", fmt.Errorf("unknown provider %q", provider)
	}
}

// startTraeLogin TRAE：临时本地监听捕获回调。
func (c *Core) startTraeLogin() (string, error) {
	ln, port, err := listenLoopback(18080)
	if err != nil {
		return "", fmt.Errorf("local callback listener: %w", err)
	}
	ctx := c.Trae.BuildLoginURL(port)
	sess := &loginSession{
		provider: auth.ProviderTrae,
		status:   "pending",
		traeCtx:  ctx,
		listener: ln,
	}
	c.login = sess

	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		res, perr := trae.ParseCallback(r.URL.Query())
		if perr != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<h3>登录回调无效，请重试</h3>"))
			sess.status, sess.err = "error", "invalid callback"
			_ = ln.Close()
			return
		}
		// ExchangeToken → GetUserInfo → 落盘
		token, newRefresh, expiresAt, terr := c.Trae.ExchangeTokenRaw(res.RefreshToken, "")
		if terr != nil {
			sess.status, sess.err = "error", "exchange token failed"
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<h3>换取令牌失败，请重试</h3>"))
			_ = ln.Close()
			return
		}
		uid, nickname, entID, _ := c.Trae.GetUserInfo(token, "")
		if uid == "" {
			uid, nickname, entID = res.UID, res.Nickname, res.EnterpriseID
		}
		if uid == "" || !auth.ValidUID(uid) {
			sess.status, sess.err = "error", "uid missing or invalid"
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<h3>未获取到用户信息，请重试</h3>"))
			_ = ln.Close()
			return
		}
		acct := &auth.Account{
			Provider:     auth.ProviderTrae,
			AccessToken:  token,
			RefreshToken: newRefresh,
			ExpiresAt:    expiresAt,
			Domain:       "trae.cn",
			ApiHost:      "",
			MachineID:    ctx.MachineID,
			DeviceID:     ctx.DeviceID,
			UID:          uid,
			EnterpriseID: entID,
			Nickname:     nickname,
		}
		if err := c.Store.Upsert(acct); err != nil {
			sess.status, sess.err = "error", "save account failed"
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<h3>保存账号失败</h3>"))
			_ = ln.Close()
			return
		}
		c.syncPool()
		sess.status = "done"
		sess.result = &LoginResult{Provider: auth.ProviderTrae, UID: uid, Nickname: nickname}
		c.logf("info", "trae account added: uid=%s", uid)

		// 回调页不回显任何敏感参数
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面返回应用</h3>"))
		_ = ln.Close()
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<h3>等待 TRAE 登录回调…</h3>"))
	})

	go func() {
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		_ = srv.Serve(ln)
	}()
	// 超时自动失效
	go func() {
		time.Sleep(loginTimeout)
		c.loginMu.Lock()
		defer c.loginMu.Unlock()
		if c.login == sess && c.login.status == "pending" {
			c.login.status, c.login.err = "error", "login timeout"
			_ = ln.Close()
		}
	}()
	return ctx.URL, nil
}

// startWorkBuddyLogin WorkBuddy：发起设备流登录。
func (c *Core) startWorkBuddyLogin() (string, error) {
	sess, err := c.WB.StartLogin()
	if err != nil {
		return "", err
	}
	c.login = &loginSession{
		provider: auth.ProviderWorkBuddy,
		status:   "pending",
		wbState:  sess,
	}
	go func() {
		time.Sleep(loginTimeout)
		c.loginMu.Lock()
		defer c.loginMu.Unlock()
		if c.login != nil && c.login.status == "pending" && c.login.provider == auth.ProviderWorkBuddy {
			c.login.status, c.login.err = "error", "login timeout"
		}
	}()
	return sess.AuthURL, nil
}

// PollLoginStatus 前端轮询。WorkBuddy 在此驱动真实轮询；TRAE 由回调监听推进。
func (c *Core) PollLoginStatus() LoginStatusResponse {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.login == nil {
		return LoginStatusResponse{}
	}
	resp := LoginStatusResponse{
		Active:   true,
		Provider: c.login.provider,
		Status:   c.login.status,
		Err:      c.login.err,
		Result:   c.login.result,
	}
	// WorkBuddy：每次轮询打一次上游 token 端点
	if c.login.provider == auth.ProviderWorkBuddy && c.login.status == "pending" && c.login.wbState != nil {
		acct, err := c.WB.PollLogin(c.login.wbState.State)
		if err != nil {
			if err == workbuddy.ErrPending {
				return resp
			}
			c.login.status, c.login.err = "error", desensitizeErr(err)
			resp.Status, resp.Err = c.login.status, c.login.err
			return resp
		}
		if err := c.Store.Upsert(acct); err != nil {
			c.login.status, c.login.err = "error", "save account failed"
			resp.Status, resp.Err = c.login.status, c.login.err
			return resp
		}
		c.syncPool()
		s := acct.Snap()
		c.login.status = "done"
		c.login.result = &LoginResult{Provider: s.Provider, UID: s.UID, Nickname: s.Nickname}
		c.logf("info", "workbuddy account added: uid=%s", s.UID)
		resp.Status = "done"
		resp.Result = c.login.result
	}
	return resp
}

// CancelLogin 取消进行中的登录。
func (c *Core) CancelLogin() {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.login != nil {
		if c.login.listener != nil {
			_ = c.login.listener.Close()
		}
		c.login = nil
	}
}

// listenLoopback 优先用 prefer 端口，被占用则随机。只绑 127.0.0.1。
func listenLoopback(prefer int) (net.Listener, int, error) {
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", prefer)); err == nil {
		return ln, prefer, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}
