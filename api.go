// api.go Wails 前端绑定桥（全部走 Core，本层只做 DTO 组装，不含业务）。
package main

import (
	"context"
	"os/exec"
	stdruntime "runtime"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"work2api-desktop/internal/app"
)

// API 前端可调用集合。
type API struct {
	core *app.Core
	ctx  context.Context
}

// Dashboard 仪表盘聚合数据。
type Dashboard struct {
	Version       string `json:"version"`
	Port          int    `json:"port"`
	APIKey        string `json:"apiKey"`
	AccountCount  int    `json:"accountCount"`
	HealthyCount  int    `json:"healthyCount"`
	TotalCredits  int64  `json:"totalCredits"`
	UptimeMs      int64  `json:"uptimeMs"`
	BaseURL       string `json:"baseUrl"`
}

// SettingsDTO 设置视图。
type SettingsDTO struct {
	Port            int    `json:"port"`
	DefaultProvider string `json:"defaultProvider"`
	CheckinEnabled  bool   `json:"checkinEnabled"`
	CheckinTime     string `json:"checkinTime"`
	StartMinimized  bool   `json:"startMinimized"`
}

// NewAPI 构建。
func NewAPI(core *app.Core) *API {
	return &API{core: core}
}

// GetDashboard 仪表盘。
func (a *API) GetDashboard() Dashboard {
	cfg := a.core.Cfg()
	accounts := a.core.Pool.List()
	var credits int64
	healthy := 0
	for _, s := range accounts {
		credits += s.Credits
		if !s.Cooling && !s.Disabled {
			healthy++
		}
	}
	return Dashboard{
		Version:      app.Version,
		Port:         cfg.Port,
		APIKey:       cfg.APIKey,
		AccountCount: len(accounts),
		HealthyCount: healthy,
		TotalCredits: credits,
		UptimeMs:     a.core.UptimeMs(),
		BaseURL:      "http://127.0.0.1:" + itoa(cfg.Port),
	}
}

// GetSettings 读取设置。
func (a *API) GetSettings() SettingsDTO {
	cfg := a.core.Cfg()
	return SettingsDTO{
		Port:            cfg.Port,
		DefaultProvider: cfg.DefaultProvider,
		CheckinEnabled:  cfg.CheckinEnabled,
		CheckinTime:     cfg.CheckinTime,
		StartMinimized:  cfg.StartMinimized,
	}
}

// SaveSettings 保存设置；返回端口是否变化（已自动重启服务）。
func (a *API) SaveSettings(s SettingsDTO) (bool, error) {
	if s.Port < 1024 || s.Port > 65535 {
		return false, errString("端口必须在 1024-65535 之间")
	}
	if s.DefaultProvider != "trae" && s.DefaultProvider != "workbuddy" && s.DefaultProvider != "auto" {
		return false, errString("默认上游必须是 trae / workbuddy / auto")
	}
	nc := app.Config{
		Port:            s.Port,
		DefaultProvider: s.DefaultProvider,
		CheckinEnabled:  s.CheckinEnabled,
		CheckinTime:     s.CheckinTime,
		StartMinimized:  s.StartMinimized,
	}
	return a.core.UpdateConfig(nc)
}

// RegenerateAPIKey 重新生成 API Key。
func (a *API) RegenerateAPIKey() (string, error) {
	return a.core.RegenerateAPIKey()
}

// ListAccounts 账号列表（脱敏）。
func (a *API) ListAccounts() []map[string]any {
	list := a.core.Pool.List()
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		out = append(out, map[string]any{
			"provider":    s.Provider,
			"uid":         s.UID,
			"nickname":    s.Nickname,
			"credits":     s.Credits,
			"cooling":     s.Cooling,
			"until":       s.Until.Format("15:04:05"),
			"reason":      s.Reason,
			"disabled":    s.Disabled,
			"expiresAt":   s.ExpiresAt,
			"expiresTime": formatUnix(s.ExpiresAt),
		})
	}
	return out
}

// DeleteAccount 删除账号。
func (a *API) DeleteAccount(provider, uid string) error {
	return a.core.DeleteAccount(provider, uid)
}

// StartLogin 发起登录，返回授权 URL。
func (a *API) StartLogin(provider string) (string, error) {
	return a.core.StartLogin(provider)
}

// PollLoginStatus 轮询登录状态。
func (a *API) PollLoginStatus() app.LoginStatusResponse {
	return a.core.PollLoginStatus()
}

// CancelLogin 取消登录。
func (a *API) CancelLogin() {
	a.core.CancelLogin()
}

// CheckinNow 手动签到。
func (a *API) CheckinNow(provider, uid string) (string, error) {
	return a.core.CheckinNow(provider, uid)
}

// RefreshCreditsNow 手动刷新积分。
func (a *API) RefreshCreditsNow(provider, uid string) (int64, error) {
	return a.core.RefreshCreditsNow(provider, uid)
}

// ReenableAccount 恢复禁用账号。
func (a *API) ReenableAccount(provider, uid string) error {
	return a.core.ReenableAccount(provider, uid)
}

// RefreshAllCredits 手动全量刷新积分。
func (a *API) RefreshAllCredits() {
	go a.core.RefreshAllCreditsPublic()
}

// GetLogs 最近 n 条日志。
func (a *API) GetLogs(n int) []app.LogEntry {
	return a.core.Log.Snapshot(n)
}

// OpenURL 用默认浏览器打开。
func (a *API) OpenURL(u string) {
	if a.ctx != nil {
		runtime.BrowserOpenURL(a.ctx, u)
	}
}

// OpenDataDir 打开数据目录（资源管理器）。
func (a *API) OpenDataDir() error {
	dir, err := app.DataDir()
	if err != nil {
		return err
	}
	if stdruntime.GOOS == "windows" {
		return exec.Command("explorer.exe", dir).Start()
	}
	return errString("unsupported platform")
}

// HideToTray 隐藏窗口到托盘。
func (a *API) HideToTray() {
	if a.ctx != nil {
		runtime.WindowHide(a.ctx)
	}
}

// QuitApp 退出应用。
func (a *API) QuitApp() {
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func formatUnix(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	t := time.Unix(sec, 0)
	return t.Format("2006-01-02 15:04")
}

type errString string

func (e errString) Error() string { return string(e) }
