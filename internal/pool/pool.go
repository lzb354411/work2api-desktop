// Package pool 统一账号池（WorkBuddy 上游）：内存状态机 + 挑号策略。
//
// 挑选策略：healthy 账号中剩余积分最多者；auto 模式跨 provider 全局挑。
// 状态（冷却/禁用/积分）仅存内存：桌面单进程语义下重启即重置，
// 积分由后台调度器周期回填，不落盘避免与 accounts.dat 双写不一致。
package pool

import (
	"sort"
	"sync"
	"time"

	"work2api-desktop/internal/auth"
)

// Key 账号唯一键。
type Key struct {
	Provider string
	UID      string
}

// Status 单账号对外状态（脱敏：不含任何 token）。
type Status struct {
	Provider  string    `json:"provider"`
	UID       string    `json:"uid"`
	Nickname  string    `json:"nickname,omitempty"`
	Credits   int64     `json:"credits"`
	Cooling   bool      `json:"cooling"`
	Until     time.Time `json:"until,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Disabled  bool      `json:"disabled"`
	ErrCount  int       `json:"errCount,omitempty"`
	ExpiresAt int64     `json:"expiresAt"`
}

type entry struct {
	a        *auth.Account
	credits  int64
	disabled bool
	reason   string
	until    time.Time
	errCount int
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	return e.until.IsZero() || !now.Before(e.until)
}

// Pool 账号池。
type Pool struct {
	mu    sync.RWMutex
	byKey map[Key]*entry
	floor int64 // 积分保留阈值（>0 时 credits <= floor 的账号不参与 chat 挑号；只读查询不受限）
}

// New 构建空池。
func New() *Pool {
	return &Pool{byKey: map[Key]*entry{}}
}

// Sync 用最新账号列表对齐池：新账号加入、消失的剔除（已有账号保留状态）。
func (p *Pool) Sync(accounts []*auth.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[Key]bool{}
	for _, a := range accounts {
		s := a.Snap()
		k := Key{s.Provider, s.UID}
		seen[k] = true
		if e, ok := p.byKey[k]; ok {
			e.a = a
		} else {
			p.byKey[k] = &entry{a: a}
		}
	}
	for k := range p.byKey {
		if !seen[k] {
			delete(p.byKey, k)
		}
	}
}

// Pick 挑可用账号（chat 路径）。provider 为空/"auto" 时跨上游全局挑；
// tried 非空时跳过其中键（单请求轮换）；
// skip 非空时跳过其中的上游（UI 禁用开关，不消耗该上游积分）；
// floor > 0 时跳过 credits <= floor 的账号（积分保留阈值：余额到线自动停用，
// 后台积分回填/签到超过阈值后自动恢复可用）。
func (p *Pool) Pick(provider string, tried map[Key]bool, skip map[string]bool) *auth.Account {
	return p.pick(provider, tried, skip, true)
}

// PickForRead 只读场景挑号（模型列表查询等不消耗积分的操作），
// 不应用积分保留阈值——账号低于阈值时仍可浏览其上游模型表。
func (p *Pool) PickForRead(provider string) *auth.Account {
	return p.pick(provider, nil, nil, false)
}

func (p *Pool) pick(provider string, tried map[Key]bool, skip map[string]bool, applyFloor bool) *auth.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	for k, e := range p.byKey {
		if provider != "" && provider != "auto" && k.Provider != provider {
			continue
		}
		if skip != nil && skip[k.Provider] {
			continue
		}
		if tried != nil && tried[k] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if applyFloor && p.floor > 0 && e.credits <= p.floor {
			continue
		}
		if best == nil || e.credits > best.credits {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

// SetCreditFloor 设置积分保留阈值（<=0 关闭）。
func (p *Pool) SetCreditFloor(floor int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if floor < 0 {
		floor = 0
	}
	p.floor = floor
}

// CreditFloor 当前积分保留阈值（0 = 未启用）。
func (p *Pool) CreditFloor() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.floor
}

// Count 返回指定 provider 的可用账号数（provider 空 = 全部）。
func (p *Pool) Count(provider string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for k, e := range p.byKey {
		if provider != "" && provider != "auto" && k.Provider != provider {
			continue
		}
		if e.healthy(now) {
			n++
		}
	}
	return n
}

// SetCredits 更新积分。
func (p *Pool) SetCredits(provider, uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[Key{provider, uid}]; ok {
		e.credits = credits
	}
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(provider, uid string, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[Key{provider, uid}]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
}

// Disable 永久禁用（session 失效），需删除重登恢复。
func (p *Pool) Disable(provider, uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[Key{provider, uid}]; ok {
		e.disabled = true
		e.reason = reason
	}
}

// Reenable 恢复禁用账号（重登后）。
func (p *Pool) Reenable(provider, uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[Key{provider, uid}]; ok {
		e.disabled = false
		e.reason = ""
		e.until = time.Time{}
		e.errCount = 0
	}
}

// NoteError 记录一次错误；达到 threshold 自动冷却 d。
func (p *Pool) NoteError(provider, uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[Key{provider, uid}]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(provider, uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[Key{provider, uid}]; ok {
		e.errCount = 0
	}
}

// List 全部账号状态（provider+uid 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	keys := make([]Key, 0, len(p.byKey))
	for k := range p.byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Provider != keys[j].Provider {
			return keys[i].Provider < keys[j].Provider
		}
		return keys[i].UID < keys[j].UID
	})
	now := time.Now()
	out := make([]Status, 0, len(keys))
	for _, k := range keys {
		e := p.byKey[k]
		s := e.a.Snap()
		out = append(out, Status{
			Provider:  k.Provider,
			UID:       k.UID,
			Nickname:  s.Nickname,
			Credits:   e.credits,
			Cooling:   !e.until.IsZero() && now.Before(e.until),
			Until:     e.until,
			Reason:    e.reason,
			Disabled:  e.disabled,
			ErrCount:  e.errCount,
			ExpiresAt: s.ExpiresAt,
		})
	}
	return out
}
