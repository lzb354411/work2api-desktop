// login.go 登录管理器：WorkBuddy 轮询设备流登录。
//
// 安全设计：
//   - 一次只允许一个登录流程（loginMu 互斥）
//   - 登录结果脱敏，不暴露任何 token
package app

import (
	"fmt"
	"time"

	"work2api-desktop/internal/auth"
	"work2api-desktop/internal/upstream/workbuddy"
)

const loginTimeout = 5 * time.Minute

// LoginStatusResponse 前端轮询的登录状态。
type LoginStatusResponse struct {
	Active   bool         `json:"active"`   // 是否有进行中的登录流程
	Provider string       `json:"provider"` // workbuddy
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
	c.login = nil

	if provider != auth.ProviderWorkBuddy {
		return "", fmt.Errorf("unknown provider %q", provider)
	}
	return c.startWorkBuddyLogin()
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

// PollLoginStatus 前端轮询。每次打一次上游 token 端点。
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
	if c.login.status == "pending" && c.login.wbState != nil {
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
	c.login = nil
}