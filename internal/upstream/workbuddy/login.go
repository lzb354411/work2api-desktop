// login.go WorkBuddy OAuth 设备流登录（CN only，服务端签发 state，无 PKCE）。
//
// 流程：
//  1. StartLogin: POST /v2/plugin/auth/state → {state, authUrl}
//  2. 用户浏览器打开 authUrl 完成登录
//  3. PollLogin: GET /v2/plugin/auth/token?state=...（未完成时业务 code != 0）
//     成功后再 GET /v2/plugin/login/account?state=... 拿 uid/nickname
//
// 安全：每次登录独立 cookie jar，多账号登录互不串会话。
package workbuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	"work2api-desktop/internal/auth"
)

// LoginSession 一次登录会话。
type LoginSession struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

// ErrPending 登录尚未完成（轮询继续）。
var ErrPending = fmt.Errorf("waiting for login")

// StartLogin 发起登录，返回授权 URL。
func (c *Client) StartLogin() (*LoginSession, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	req, err := http.NewRequest(http.MethodPost, c.ChatBaseCN+"/v2/plugin/auth/state?platform=CLI", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	CommonHeaders(req, "cn")
	data, err := c.doJSONPlain(client, req)
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl")
	}
	return &LoginSession{State: st.State, AuthURL: st.AuthURL}, nil
}

// PollLogin 轮询登录状态。未完成返回 ErrPending；完成返回完整账号。
func (c *Client) PollLogin(state string) (*auth.Account, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	req, err := http.NewRequest(http.MethodGet, c.ChatBaseCN+"/v2/plugin/auth/token?state="+url.QueryEscape(state), nil)
	if err != nil {
		return nil, err
	}
	CommonHeaders(req, "cn")
	tokRaw, err := c.doJSONPlain(client, req)
	if err != nil {
		return nil, ErrPending
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return nil, ErrPending
	}

	// login/account 拿 uid/nickname（带 Bearer）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	req2, err := http.NewRequest(http.MethodGet, c.ChatBaseCN+"/v2/plugin/login/account?state="+url.QueryEscape(state), nil)
	if err == nil {
		CommonHeaders(req2, "cn")
		req2.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if acctRaw, err2 := c.doJSONPlain(client, req2); err2 == nil {
			_ = json.Unmarshal(acctRaw, &acct)
		}
	}
	if acct.UID == "" {
		return nil, fmt.Errorf("login done but uid missing")
	}
	if !auth.ValidUID(acct.UID) {
		return nil, fmt.Errorf("invalid uid from upstream")
	}
	expiresAt := int64(0)
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return &auth.Account{
		Provider:     auth.ProviderWorkBuddy,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiresAt,
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
	}, nil
}

// doJSONPlain 带独立 client 的信封解包（登录流程专用）。
func (c *Client) doJSONPlain(client *http.Client, req *http.Request) (json.RawMessage, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, nil
}
