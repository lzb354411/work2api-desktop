// client.go TRAE SOLO 上游客户端。
//
// 相对 traework2api 的安全加固：
//   - 日志脱敏：只记录 uid + 错误分类 + 状态码，不打印上游响应体原文
//     （原版把 body 截断 200 字符写日志，可能带出回显 token）
//   - 请求头构造全部基于 Snapshot，修复数据竞争
//   - 流读取加空闲超时包装（见 sse.go watchdog）
package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"work2api-desktop/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrPlanLimit                  // 1005 + plan → 权益不足（硬冷却 12h）
	ErrSoftRate                   // 429 → 短冷却 60s
	ErrSessionDead                // 401 → 禁用
	ErrNotFound                   // 404 → 短冷却不累计 errCount
	ErrServer                     // 5xx
	ErrClient                     // 其他 4xx
)

func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
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

// Error 带分类的上游错误。Msg 仅用于服务端日志，不会透传给 API 调用方。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return ErrPlanLimit
	}
	if status == http.StatusUnauthorized {
		return ErrSessionDead
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

// Client SOLO 上游 HTTP 客户端。
type Client struct {
	HTTP       *http.Client // 短 JSON 请求
	StreamHTTP *http.Client // SSE 流式（无总超时，靠 idle watchdog 兜底）

	AgentHost string
	UgHost    string
	OAuthHost string
	ClientID  string

	// log 写入脱敏日志（nil 时静默）。
	log func(format string, args ...any)
}

// LogFunc 脱敏日志函数类型。
type LogFunc func(format string, args ...any)

// New 生产默认值。
func New(log LogFunc) *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
		log:        log,
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.log != nil {
		c.log(format, args...)
	}
}

// doJSON 发请求并解 JSON；HTTP 非 2xx 时返回带 body 片段的 *Error（仅进日志）。
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
	return raw, nil
}

// RefreshToken 强制刷新 access token（refreshToken 轮换）。全程持写锁。
func (c *Client) RefreshToken(a *auth.Account) error {
	a.Lock()
	defer a.Unlock()
	return c.refreshLocked(a)
}

// RefreshTokenIfNeeded 仅当 token 在 skew 内即将过期时才刷新。持锁内重查。
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

// refreshLocked 持锁内部实现；失败路径不改写任何字段。
func (c *Client) refreshLocked(a *auth.Account) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.OAuthHost
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流。
// 返回的 rc 已包 idle watchdog；非 2xx 时 rc 为 nil、respBody 供 Classify。
func (c *Client) ChatStream(a *auth.Account, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	s := a.Snap()
	req, err := http.NewRequest(http.MethodPost, c.AgentHost+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, s, true)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		c.logf("chat_stream uid=%s: transport error: %v", s.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		// 脱敏：只记 uid/状态码/分类，不落 body 原文
		c.logf("chat_stream uid=%s: upstream %d %s", s.UID, resp.StatusCode, kind)
		return nil, resp.StatusCode, raw, nil
	}
	return newIdleWatchdog(resp.Body, streamIdleTimeout), resp.StatusCode, nil, nil
}

// streamIdleTimeout SSE 流空闲超时：超过该时长无任何字节到达则强制断开，
// 防止上游停滞导致 goroutine 与连接永久悬挂（原版缺失的 DoS 面）。
const streamIdleTimeout = 5 * time.Minute

// idleWatchdog 包装 ReadCloser，读超时由后台 timer 实现。
type idleWatchdog struct {
	rc      io.ReadCloser
	timeout time.Duration
	resetCh chan struct{}
	once    sync.Once
}

func newIdleWatchdog(rc io.ReadCloser, timeout time.Duration) *idleWatchdog {
	w := &idleWatchdog{rc: rc, timeout: timeout, resetCh: make(chan struct{}, 1)}
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				w.rc.Close() // 触发底层读返回错误
				return
			case <-w.resetCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(timeout)
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
	w.once.Do(func() { close(w.resetCh) })
	return w.rc.Close()
}

// ModelInfo 动态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
}

// FetchModels 拉 SOLO 模型表（get_detail_param）。
//
// 响应包含全部 37 个配置，其中大量是内部子代理模型（explore_sub_agent_*、
// browser_use_subagent、summary、内部代号 sagitta/aquila、重复别名等），
// 桌面客户端按 is_invisible_to_user + 显示名过滤，此处采用相同规则。
//
// 上下文窗口取 context_window_tokens 各键最大值（如 dev:300000 max:300000）；
// 该字段缺失时按实测规律 context = prompt_max_tokens + max_tokens 估算。
func (c *Client) FetchModels(a *auth.Account) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.AgentHost+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a.Snap(), false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName          string `json:"config_name"`
			IsInvisibleToUser   bool   `json:"is_invisible_to_user"`
			ContextWindowTokens map[string]int64 `json:"context_window_tokens"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
			ModelDetailList []struct {
				MaxTokens       int64 `json:"max_tokens"`
				PromptMaxTokens int64 `json:"prompt_max_tokens"`
			} `json:"model_detail_list"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		if cfg.ConfigName == "" || cfg.IsInvisibleToUser || cfg.DisplayConfig.DisplayName == "" {
			continue // 内部子代理/无显示名配置，桌面客户端同样不展示
		}
		var maxOut, promptMax int64
		if len(cfg.ModelDetailList) > 0 {
			maxOut = cfg.ModelDetailList[0].MaxTokens
			promptMax = cfg.ModelDetailList[0].PromptMaxTokens
		}
		ctx := int64(0)
		for _, v := range cfg.ContextWindowTokens {
			if v > ctx {
				ctx = v
			}
		}
		if ctx == 0 {
			ctx = promptMax + maxOut // 实测规律：缺失时 context = prompt_max + max_out
		}
		out = append(out, ModelInfo{
			ID:            cfg.ConfigName,
			Name:          cfg.DisplayConfig.DisplayName,
			ContextWindow: ctx,
			MaxTokens:     maxOut,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// CheckinStatus 查询签到状态。raw 返回原始响应（截断），供诊断日志。
// 业务码 code!=0 视为状态查询失败（checked_in/credits/enable 不可信），返回错误而非零值误判。
func (c *Client) CheckinStatus(a *auth.Account) (checkedIn bool, credits int64, enable bool, raw string, err error) {
	req, err := http.NewRequest(http.MethodPost, c.UgHost+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, "", err
	}
	UgHeaders(req, a.Snap())
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, "", err
	}
	var resp struct {
		CheckedIn bool   `json:"checked_in"`
		Credits   int64  `json:"credits"`
		Enable    bool   `json:"enable"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, truncate(string(data), 300), fmt.Errorf("checkin status parse: %w", err)
	}
	raw = truncate(string(data), 300)
	if resp.Code != 0 {
		msg := resp.Message
		if msg == "" {
			msg = fmt.Sprintf("code=%d", resp.Code)
		}
		return false, 0, false, raw, fmt.Errorf("checkin status failed: %s", msg)
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, raw, nil
}

// ErrCheckinBusy 签到领取瞬时繁忙（code=9074 参与用户太多），可稍后重试。
var ErrCheckinBusy = errors.New("checkin busy")

// CheckinClaim 执行签到。raw 返回原始响应（截断），供诊断日志。
// 业务码 code!=0 视为领取失败，返回真实结果而非"假成功"；9074 映射为 ErrCheckinBusy。
func (c *Client) CheckinClaim(a *auth.Account) (raw string, err error) {
	req, err := http.NewRequest(http.MethodPost, c.UgHost+EpCheckinClaim, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	UgHeaders(req, a.Snap())
	data, err := c.doJSON(req)
	if err != nil {
		return "", err
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return truncate(string(data), 300), fmt.Errorf("checkin claim parse: %w", err)
	}
	raw = truncate(string(data), 300)
	if resp.Code != 0 {
		msg := resp.Message
		if msg == "" {
			msg = fmt.Sprintf("code=%d", resp.Code)
		}
		if resp.Code == 9074 {
			return raw, fmt.Errorf("%w：%s", ErrCheckinBusy, msg)
		}
		return raw, fmt.Errorf("checkin claim failed: %s", msg)
	}
	return raw, nil
}

// UserEntUsage 聚合剩余积分：remain = Σ(credits_limit - usage.credits_amount)。
// credits_limit 是权益包总额度，usage.credits_amount 是已用积分（实测字段）；
// 只累加 limit 会把已耗尽的账号误显示为满额度。
func (c *Client) UserEntUsage(a *auth.Account) (remain int64, err error) {
	req, err := http.NewRequest(http.MethodPost, c.UgHost+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, err
	}
	UgHeaders(req, a.Snap())
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("ent usage parse: %w", err)
	}
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		remain += l - int64(p.Usage.CreditsAmount)
	}
	if remain < 0 {
		remain = 0
	}
	return remain, nil
}

// GetUserInfo 查询账号信息（登录用）。
func (c *Client) GetUserInfo(accessToken, apiHost string) (uid, nickname, enterpriseID string, err error) {
	host := apiHost
	if host == "" {
		host = c.OAuthHost
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", accessToken)
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

// ExchangeTokenRaw 用回调拿到的 refreshToken 换 token（登录流程用，非刷新路径）。
func (c *Client) ExchangeTokenRaw(refreshToken, apiHost string) (token, newRefresh string, expiresAt int64, err error) {
	host := apiHost
	if host == "" {
		host = c.OAuthHost
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return "", "", 0, err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", 0, err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", 0, fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return "", "", 0, fmt.Errorf("no token in exchange response")
	}
	expiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	if expiresAt <= 0 && resp.Result.TokenExpireDuration > 0 {
		expiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return resp.Result.Token, resp.Result.RefreshToken, expiresAt, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
