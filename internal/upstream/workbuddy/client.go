// client.go WorkBuddy 上游客户端（chat / billing / refresh / models）。
//
// 相对 workbuddy2api 的安全加固：
//   - 日志脱敏：只记 uid + 分类 + 状态码，不打印上游 body 原文
//   - 请求头全部基于 Snapshot（修复原版无锁读 AccessToken 的数据竞争）
//   - SSE 流包 idle watchdog
package workbuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"work2api-desktop/internal/auth"
)

// LogFunc 脱敏日志函数类型。
type LogFunc func(format string, args ...any)

// ErrKind 错误分类。
type ErrKind int

const (
	ErrNone        ErrKind = iota
	ErrHardCredit          // 余额不足 → 长冷却
	ErrSoftRate            // 429 → 短冷却
	ErrSessionDead         // 12153 → 禁用
	ErrNotFound            // 404 → 短冷却不累计 errCount
	ErrServer              // 5xx
	ErrClient              // 其他 4xx
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。Msg 仅用于服务端日志。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// Classify 按状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。
type Client struct {
	HTTP *http.Client

	ChatBaseCN      string
	BillingBaseCN   string
	ChatBaseGlobal  string
	BillingBaseGlob string

	log LogFunc
}

// New 生产默认值。
func New(log LogFunc) *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		HTTP:            &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatBaseCN:      ChatBaseCN,
		BillingBaseCN:   BillingBaseCN,
		ChatBaseGlobal:  ChatBaseGlobal,
		BillingBaseGlob: BillingBaseGlob,
		log:             log,
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.log != nil {
		c.log(format, args...)
	}
}

func (c *Client) chatBase(s auth.Snapshot) string {
	if s.Provider == auth.ProviderWorkBuddy && isGlobalDomain(s.Domain) {
		return c.ChatBaseGlobal
	}
	return c.ChatBaseCN
}

func (c *Client) billingBase(s auth.Snapshot) string {
	if s.Provider == auth.ProviderWorkBuddy && isGlobalDomain(s.Domain) {
		return c.BillingBaseGlob
	}
	return c.BillingBaseCN
}

func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token。全程持写锁；缺省值保留旧值。
func (c *Client) RefreshToken(a *auth.Account) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	s := a.Snap() // 持写锁时快照安全
	url := c.chatBase(s) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	RefreshHeaders(req, s)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// RefreshTokenIfNeeded 仅当 token 临近过期时刷新。
func (c *Client) RefreshTokenIfNeeded(a *auth.Account, skew time.Duration) (bool, error) {
	a.Lock()
	defer a.Unlock()
	if !a.NeedsRefreshLocked(skew) {
		return false, nil
	}
	if err := c.refreshLocked(a); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) refreshLocked(a *auth.Account) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	s := a.Snap()
	url := c.chatBase(s) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	RefreshHeaders(req, s)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（已包 idle watchdog）。
func (c *Client) ChatStream(a *auth.Account, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	s := a.Snap()
	url := c.chatBase(s) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	ChatHeaders(req, s)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.logf("chat_stream uid=%s: transport error: %v", s.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		c.logf("chat_stream uid=%s: upstream %d %s", s.UID, resp.StatusCode, kind)
		return nil, resp.StatusCode, raw, nil
	}
	return newIdleWatchdog(resp.Body, 5*time.Minute), resp.StatusCode, nil, nil
}

// idleWatchdog 读空闲超时强制断开 SSE 连接。
type idleWatchdog struct {
	rc      io.ReadCloser
	timeout time.Duration
	resetCh chan struct{}
	closed  chan struct{}
}

func newIdleWatchdog(rc io.ReadCloser, timeout time.Duration) *idleWatchdog {
	w := &idleWatchdog{rc: rc, timeout: timeout, resetCh: make(chan struct{}, 1), closed: make(chan struct{})}
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				w.rc.Close()
				return
			case <-w.resetCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(timeout)
			case <-w.closed:
				return
			}
		}
	}()
	return w
}

func (w *idleWatchdog) Read(p []byte) (int, error) {
	n, err := w.rc.Read(p)
	if n > 0 {
		select {
		case w.resetCh <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (w *idleWatchdog) Close() error {
	select {
	case <-w.closed:
	default:
		close(w.closed)
	}
	return w.rc.Close()
}

// ModelInfo 动态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
}

// FetchModels 调上游动态模型接口（agents.cli 交集）。
func (c *Client) FetchModels(a *auth.Account) ([]ModelInfo, error) {
	s := a.Snap()
	url := c.chatBase(s) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	region := "cn"
	if isGlobalDomain(s.Domain) {
		region = "global"
	}
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(region)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", ClientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d", resp.StatusCode)
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	type minfo struct {
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		Disabled        bool
	}
	dynMap := map[string]minfo{}
	for _, m := range env.Data.Models {
		dynMap[m.ID] = minfo{m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, ModelInfo{
			ID:            id,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// UserResource 查询账号当前可花费积分余额。
func (c *Client) UserResource(a *auth.Account) (remain int64, err error) {
	s := a.Snap()
	url := c.billingBase(s) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	BillingHeaders(req, s)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					CycleCapacitySize   int64 `json:"CycleCapacitySize"`
					CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
					CapacityRemain      int64 `json:"CapacityRemain"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
	}
	return remain, nil
}

// DailyCheckin 执行每日签到。
func (c *Client) DailyCheckin(a *auth.Account) error {
	s := a.Snap()
	url := c.billingBase(s) + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	BillingHeaders(req, s)
	_, err = c.doJSON(req)
	return err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
