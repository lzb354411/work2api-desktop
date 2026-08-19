// Package auth 统一账号凭据模型（TRAE SOLO + WorkBuddy 双上游）与加密存储。
//
// 安全设计：
//   - 凭据整体存入 accounts.dat（DPAPI 静态加密，绑定当前 Windows 用户）
//   - 可变字段（AccessToken/RefreshToken/ExpiresAt）由 mu 保护，
//     读路径统一走 Snapshot() 快照，写路径（token 刷新）持写锁整段执行——
//     修复旧版 workbuddy2api 请求头无锁读 AccessToken 的数据竞争
//   - UID 严格白名单校验（^[A-Za-z0-9_-]{1,64}$），杜绝旧版 login.sh 的路径遍历问题
package auth

import (
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Provider 上游类型。
const (
	ProviderTrae      = "trae"
	ProviderWorkBuddy = "workbuddy"
)

var uidPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidUID 校验 UID 合法性（白名单：字母数字下划线短横线，1-64 字符）。
func ValidUID(uid string) bool { return uidPattern.MatchString(uid) }

// Account 归一化账号凭证。
type Account struct {
	mu sync.RWMutex

	Provider     string // trae | workbuddy
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64  // Unix 秒
	Domain       string // workbuddy 区域判定 / trae 固定 "trae.cn"
	ApiHost      string // trae: ExchangeToken host
	MachineID    string // trae: x-machine-id
	DeviceID     string // trae: x-device-id
	UID          string
	EnterpriseID string
	Nickname     string
}

// Snapshot 持读锁拷贝全部字段，返回值类型快照。
// 所有需要读 token 的请求构造路径必须使用快照，禁止直接读字段。
type Snapshot struct {
	Provider     string
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Domain       string
	ApiHost      string
	MachineID    string
	DeviceID     string
	UID          string
	EnterpriseID string
	Nickname     string
}

func (a *Account) Snap() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return Snapshot{
		Provider:     a.Provider,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		ApiHost:      a.ApiHost,
		MachineID:    a.MachineID,
		DeviceID:     a.DeviceID,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
	}
}

// Lock/Unlock 供 upstream 刷新路径持写锁整段执行。
func (a *Account) Lock()   { a.mu.Lock() }
func (a *Account) Unlock() { a.mu.Unlock() }

// NeedsRefreshLocked 持锁版本；调用方必须已持有 a.mu。
func (a *Account) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// NeedsRefresh 报告 token 是否将在 within 内过期。
func (a *Account) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// Region 返回 workbuddy 区域（cn/global）；trae 账号返回空。
func (a *Account) Region() string {
	s := a.Snap()
	if s.Provider != ProviderWorkBuddy {
		return ""
	}
	d := s.Domain
	if d == "workbuddy.ai" || (len(d) > len("workbuddy.ai") && d[len(d)-len(".workbuddy.ai"):] == ".workbuddy.ai") {
		return "global"
	}
	return "cn"
}

// ---------------------------------------------------------------------------
// 序列化（accounts.dat 的 JSON 形态；文件本身整体 DPAPI 加密）
// ---------------------------------------------------------------------------

type accountJSON struct {
	Provider     string `json:"provider"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain,omitempty"`
	ApiHost      string `json:"apiHost,omitempty"`
	MachineID    string `json:"machineId,omitempty"`
	DeviceID     string `json:"deviceId,omitempty"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
}

// Marshal 持锁导出 JSON 形态。
func (a *Account) Marshal() accountJSON {
	s := a.Snap()
	return accountJSON{
		Provider:     s.Provider,
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
		ExpiresAt:    s.ExpiresAt,
		Domain:       s.Domain,
		ApiHost:      s.ApiHost,
		MachineID:    s.MachineID,
		DeviceID:     s.DeviceID,
		UID:          s.UID,
		EnterpriseID: s.EnterpriseID,
		Nickname:     s.Nickname,
	}
}

// UnmarshalAccount 从 JSON 形态还原。
func UnmarshalAccount(j accountJSON) (*Account, error) {
	if j.Provider != ProviderTrae && j.Provider != ProviderWorkBuddy {
		return nil, fmt.Errorf("unknown provider %q", j.Provider)
	}
	if j.UID != "" && !ValidUID(j.UID) {
		return nil, fmt.Errorf("invalid uid %q", j.UID)
	}
	if j.AccessToken == "" {
		return nil, fmt.Errorf("account %s: missing accessToken", j.UID)
	}
	return &Account{
		Provider:     j.Provider,
		AccessToken:  j.AccessToken,
		RefreshToken: j.RefreshToken,
		ExpiresAt:    j.ExpiresAt,
		Domain:       j.Domain,
		ApiHost:      j.ApiHost,
		MachineID:    j.MachineID,
		DeviceID:     j.DeviceID,
		UID:          j.UID,
		EnterpriseID: j.EnterpriseID,
		Nickname:     j.Nickname,
	}, nil
}
