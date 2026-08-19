// core.go 应用核心：装配 store/pool/上游客户端，管理 API 服务与后台调度。
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"work2api-desktop/internal/auth"
	"work2api-desktop/internal/pool"
	"work2api-desktop/internal/server"
	"work2api-desktop/internal/upstream/trae"
	"work2api-desktop/internal/upstream/workbuddy"
)

// Core 应用核心。
type Core struct {
	mu      sync.RWMutex
	cfg     *Config
	cfgPath string

	Store *auth.Store
	Pool  *pool.Pool
	Trae  *trae.Client
	WB    *workbuddy.Client
	Log   *RingLog

	srv       *http.Server
	apiH      *server.Handler // 当前 API handler（模型列表查询用）
	startedAt time.Time

	// 登录会话（单并发：一次只允许一个登录流程）
	loginMu sync.Mutex
	login   *loginSession
}

type loginSession struct {
	provider string // trae | workbuddy
	status   string // pending | done | error
	err      string
	// TRAE 专用
	traeCtx   *trae.LoginContext
	listener  net.Listener
	// WorkBuddy 专用
	wbState *workbuddy.LoginSession
	// 完成的账号（脱敏视图）
	result *LoginResult
}

// LoginResult 登录结果（脱敏：不含 token）。
type LoginResult struct {
	Provider string `json:"provider"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
}

// NewCore 装配核心。
func NewCore() (*Core, error) {
	dir, err := DataDir()
	if err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	cfg, err := LoadConfig(dir + "/config.dat")
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	store, err := auth.LoadStore(dir + "/accounts.dat")
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}

	log := NewRingLog(500)
	c := &Core{
		cfg:     cfg,
		cfgPath: dir + "/config.dat",
		Store:   store,
		Pool:    pool.New(),
		Trae:    trae.New(func(f string, a ...any) { log.Logf("info", f, a...) }),
		WB:      workbuddy.New(func(f string, a ...any) { log.Logf("info", f, a...) }),
		Log:     log,
	}
	c.syncPool()
	c.Pool.SetCreditFloor(cfg.CreditFloor)
	return c, nil
}

func (c *Core) syncPool() {
	c.Pool.Sync(c.Store.List())
}

// Cfg 读取配置副本。
func (c *Core) Cfg() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return *c.cfg
}

func (c *Core) logf(level, format string, args ...any) {
	c.Log.Logf(level, format, args...)
}

// ---------------------------------------------------------------------------
// API 服务生命周期
// ---------------------------------------------------------------------------

// Start 启动 API 服务与后台调度。
func (c *Core) Start() error {
	c.mu.RLock()
	port := c.cfg.Port
	c.mu.RUnlock()
	if err := c.startServerLocked(port); err != nil {
		return err
	}
	c.startedAt = time.Now()
	c.refreshAllCredits()
	go c.tokenRefreshLoop()
	go c.creditsLoop()
	go c.checkinLoop()
	c.logf("info", "core started: api on 127.0.0.1:%d, accounts=%d", port, len(c.Store.List()))
	return nil
}

// Stop 停止（程序退出时）。
func (c *Core) Stop() {
	c.mu.Lock()
	srv := c.srv
	c.srv = nil
	c.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	c.loginMu.Lock()
	if c.login != nil && c.login.listener != nil {
		_ = c.login.listener.Close()
	}
	c.loginMu.Unlock()
}

func (c *Core) startServerLocked(port int) error {
	h := server.New(server.Deps{
		Pool:  c.Pool,
		Store: c.Store,
		Trae:  c.Trae,
		WB:    c.WB,
		APIKey: func() string {
			c.mu.RLock()
			defer c.mu.RUnlock()
			return c.cfg.APIKey
		},
		DefaultProvider: func() string {
			c.mu.RLock()
			defer c.mu.RUnlock()
			return c.cfg.DefaultProvider
		},
		ProviderEnabled: func(provider string) bool {
			c.mu.RLock()
			defer c.mu.RUnlock()
			return c.cfg.ProviderEnabled(provider)
		},
		Logf: func(f string, a ...any) { c.logf("info", f, a...) },
	})
	// 安全：只监听 loopback，不暴露局域网
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen 127.0.0.1:%d: %w", port, err)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	c.mu.Lock()
	c.srv = srv
	c.apiH = h
	c.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.logf("error", "api server stopped: %v", err)
		}
	}()
	return nil
}

// RestartServer 端口变更后重启监听。
func (c *Core) RestartServer() error {
	c.mu.Lock()
	old := c.srv
	c.srv = nil
	port := c.cfg.Port
	c.mu.Unlock()
	if old != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = old.Shutdown(ctx)
	}
	return c.startServerLocked(port)
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// UpdateConfig 更新配置并按需重启服务。返回是否需要重启（端口变化）。
func (c *Core) UpdateConfig(nc Config) (bool, error) {
	c.mu.Lock()
	portChanged := nc.Port != c.cfg.Port
	autoStartChanged := nc.AutoStart != c.cfg.AutoStart
	floorChanged := nc.CreditFloor != c.cfg.CreditFloor
	nc.APIKey = c.cfg.APIKey // API Key 不走此路径修改
	c.cfg = &nc
	c.mu.Unlock()
	if err := SaveConfig(c.cfgPath, &nc); err != nil {
		return false, err
	}
	if floorChanged {
		c.Pool.SetCreditFloor(nc.CreditFloor)
		if nc.CreditFloor > 0 {
			c.logf("info", "credit floor set to %d: accounts at/below paused until credits recover", nc.CreditFloor)
		} else {
			c.logf("info", "credit floor disabled")
		}
	}
	if autoStartChanged {
		if err := SetAutoStart(nc.AutoStart); err != nil {
			c.logf("error", "autostart: %v", err)
		} else if nc.AutoStart {
			c.logf("info", "autostart enabled")
		} else {
			c.logf("info", "autostart disabled")
		}
	}
	if portChanged {
		if err := c.RestartServer(); err != nil {
			return true, err
		}
	}
	return portChanged, nil
}

// RegenerateAPIKey 重新生成 API Key。
func (c *Core) RegenerateAPIKey() (string, error) {
	c.mu.Lock()
	c.cfg.APIKey = GenerateAPIKey()
	key := c.cfg.APIKey
	cfg := *c.cfg
	c.mu.Unlock()
	if err := SaveConfig(c.cfgPath, &cfg); err != nil {
		return "", err
	}
	c.logf("info", "api key regenerated")
	return key, nil
}

// ---------------------------------------------------------------------------
// 后台调度
// ---------------------------------------------------------------------------

const (
	refreshSkew  = 10 * time.Minute
	refreshEvery = 5 * time.Minute
	creditsEvery = 30 * time.Minute
	errCooldown  = 10 * time.Minute

	// 签到领取瞬时繁忙(9074)时的重试：次数与间隔
	checkinClaimRetries   = 3
	checkinClaimRetryWait = 5 * time.Second
)

func (c *Core) tokenRefreshLoop() {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for range t.C {
		c.refreshAllTokens()
	}
}

func (c *Core) refreshAllTokens() {
	changed := false
	for _, a := range c.Store.List() {
		s := a.Snap()
		var err error
		switch s.Provider {
		case auth.ProviderTrae:
			_, err = c.Trae.RefreshTokenIfNeeded(a, refreshSkew)
		case auth.ProviderWorkBuddy:
			_, err = c.WB.RefreshTokenIfNeeded(a, refreshSkew)
		default:
			continue
		}
		if err != nil {
			c.logf("warn", "refresh %s/%s failed: %v", s.Provider, s.UID, desensitizeErr(err))
			c.Pool.Cooldown(s.Provider, s.UID, errCooldown, "token refresh failed")
			continue
		}
		changed = true
	}
	if changed {
		_ = c.Store.Save()
	}
}

func (c *Core) creditsLoop() {
	t := time.NewTicker(creditsEvery)
	defer t.Stop()
	for range t.C {
		c.refreshAllCredits()
	}
}

// refreshAllCredits 刷新全部账号积分。
func (c *Core) refreshAllCredits() {
	for _, a := range c.Store.List() {
		s := a.Snap()
		credits, err := c.fetchCredits(a)
		if err != nil {
			continue
		}
		c.Pool.SetCredits(s.Provider, s.UID, credits)
	}
}

func (c *Core) fetchCredits(a *auth.Account) (int64, error) {
	s := a.Snap()
	switch s.Provider {
	case auth.ProviderTrae:
		remain, err := c.Trae.UserEntUsage(a)
		if err != nil {
			return 0, err
		}
		// 显示积分 = 权益包剩余额度 + 签到积分池（两套独立积分体系）
		_, checkin, _, _, err := c.Trae.CheckinStatus(a)
		if err != nil {
			c.logf("warn", "checkin credits %s query failed: %v", s.UID, desensitizeErr(err))
			return remain, nil
		}
		return remain + checkin, nil
	case auth.ProviderWorkBuddy:
		return c.WB.UserResource(a)
	}
	return 0, fmt.Errorf("unknown provider")
}

// checkinLoop 每日定时签到。
func (c *Core) checkinLoop() {
	for {
		c.mu.RLock()
		enabled := c.cfg.CheckinEnabled
		hhmm := c.cfg.CheckinTime
		c.mu.RUnlock()
		if !enabled {
			time.Sleep(time.Minute)
			continue
		}
		next, err := nextRunTime(hhmm)
		if err != nil {
			time.Sleep(time.Minute) // 配置非法，稍后重试（配置修正后生效）
			continue
		}
		time.Sleep(time.Until(next))
		c.runCheckinAll()
	}
}

func (c *Core) runCheckinAll() {
	c.mu.RLock()
	enabled := c.cfg.CheckinEnabled
	c.mu.RUnlock()
	if !enabled {
		return
	}
	for _, a := range c.Store.List() {
		s := a.Snap()
		if _, err := c.checkinOne(a); err != nil {
			c.logf("warn", "checkin %s/%s failed: %v", s.Provider, s.UID, desensitizeErr(err))
		}
	}
	c.refreshAllCredits()
}

// checkinOne 单账号签到，返回结果描述（供 UI 提示与诊断）。
// TRAE：enable=false 或已签到时不调用领取接口，明确告知而非静默"成功"。
func (c *Core) checkinOne(a *auth.Account) (string, error) {
	s := a.Snap()
	switch s.Provider {
	case auth.ProviderTrae:
		checked, _, enable, raw, err := c.Trae.CheckinStatus(a)
		if err != nil {
			return "", err
		}
		c.logf("info", "checkin status %s: %s", s.UID, raw)
		if !enable {
			return "签到未生效（接口返回 enable=false，详见日志）", nil
		}
		if checked {
			return "今日已签到，无需重复", nil
		}
		claimRaw, err := c.Trae.CheckinClaim(a)
		if err != nil {
			c.logf("warn", "checkin claim %s failed: %v", s.UID, desensitizeErr(err))
			// 9074（参与用户太多）为瞬时繁忙，短暂重试后再如实上报
			if errors.Is(err, trae.ErrCheckinBusy) {
				for i := 0; i < checkinClaimRetries; i++ {
					time.Sleep(checkinClaimRetryWait)
					claimRaw, err = c.Trae.CheckinClaim(a)
					if err == nil {
						break
					}
					c.logf("warn", "checkin claim %s retry %d/%d: %v", s.UID, i+1, checkinClaimRetries, desensitizeErr(err))
				}
			}
			if err != nil {
				return "", err
			}
		}
		c.logf("info", "checkin claim %s: %s", s.UID, claimRaw)
		return "签到成功", nil
	case auth.ProviderWorkBuddy:
		if err := c.WB.DailyCheckin(a); err != nil {
			return "", err
		}
		return "签到成功", nil
	}
	return "", fmt.Errorf("unknown provider")
}

func nextRunTime(hhmm string) (time.Time, error) {
	var h, m int
	if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil || h > 23 || m > 59 {
		return time.Time{}, fmt.Errorf("bad time format")
	}
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next, nil
}

func desensitizeErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// ---------------------------------------------------------------------------
// 账号操作
// ---------------------------------------------------------------------------

// DeleteAccount 删除账号。
func (c *Core) DeleteAccount(provider, uid string) error {
	if !auth.ValidUID(uid) {
		return fmt.Errorf("invalid uid")
	}
	if err := c.Store.Remove(provider, uid); err != nil {
		return err
	}
	c.syncPool()
	c.logf("info", "account removed: %s/%s", provider, uid)
	return nil
}

// CheckinNow 手动签到单个账号，返回结果描述。
func (c *Core) CheckinNow(provider, uid string) (string, error) {
	a := c.findAccount(provider, uid)
	if a == nil {
		return "", fmt.Errorf("account not found")
	}
	msg, err := c.checkinOne(a)
	if err != nil {
		return "", err
	}
	c.refreshAllCredits()
	return msg, nil
}

// CheckinAllNow 一键全签所有账号，返回统计摘要。
func (c *Core) CheckinAllNow() (string, error) {
	var ok, skip, fail int
	for _, a := range c.Store.List() {
		msg, err := c.checkinOne(a)
		if err != nil {
			fail++
			s := a.Snap()
			c.logf("warn", "checkin %s/%s failed: %v", s.Provider, s.UID, desensitizeErr(err))
			continue
		}
		if msg == "签到成功" {
			ok++
		} else {
			skip++
		}
	}
	c.refreshAllCredits()
	if fail > 0 {
		return fmt.Sprintf("全部签到完成：成功 %d，跳过 %d，失败 %d（详见日志）", ok, skip, fail), nil
	}
	return fmt.Sprintf("全部签到完成：成功 %d，跳过 %d（已签到/未生效）", ok, skip), nil
}

// RefreshCreditsNow 手动刷新单个账号积分。
func (c *Core) RefreshCreditsNow(provider, uid string) (int64, error) {
	a := c.findAccount(provider, uid)
	if a == nil {
		return 0, fmt.Errorf("account not found")
	}
	credits, err := c.fetchCredits(a)
	if err != nil {
		return 0, err
	}
	c.Pool.SetCredits(provider, uid, credits)
	return credits, nil
}

// RefreshAllCreditsPublic 手动全量刷新积分（API 桥调用）。
func (c *Core) RefreshAllCreditsPublic() {
	c.refreshAllCredits()
}

// ReenableAccount 恢复被禁用的账号。
func (c *Core) ReenableAccount(provider, uid string) error {
	c.Pool.Reenable(provider, uid)
	return nil
}

func (c *Core) findAccount(provider, uid string) *auth.Account {
	for _, a := range c.Store.List() {
		s := a.Snap()
		if s.Provider == provider && s.UID == uid {
			return a
		}
	}
	return nil
}

// UptimeMs 运行时长。
func (c *Core) UptimeMs() int64 {
	if c.startedAt.IsZero() {
		return 0
	}
	return time.Since(c.startedAt).Milliseconds()
}

// ---------------------------------------------------------------------------
// 模型列表
// ---------------------------------------------------------------------------

// Models 两个上游各自的模型列表（动态优先，静态兜底）。
func (c *Core) Models() (traeModels, wbModels []server.ModelBrief) {
	c.mu.RLock()
	h := c.apiH
	c.mu.RUnlock()
	if h == nil {
		return nil, nil
	}
	return h.ListProviderModels(auth.ProviderTrae), h.ListProviderModels(auth.ProviderWorkBuddy)
}

// RefreshModels 清空缓存强制重拉模型列表。
func (c *Core) RefreshModels() {
	c.mu.RLock()
	h := c.apiH
	c.mu.RUnlock()
	if h != nil {
		h.RefreshModels()
	}
}
