// store.go 账号加密存储：accounts.dat（DPAPI 整体加密的 JSON）。
// 同时支持导入旧版 workbuddy2api 的明文 auths/*.json。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"work2api-desktop/internal/secrets"
)

// Store 账号存储。
type Store struct {
	mu       sync.RWMutex
	path     string
	accounts []*Account
}

// LoadStore 加载（或初始化空）存储。
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := secrets.OpenFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("load accounts: %w", err)
	}
	var js []accountJSON
	if err := json.Unmarshal(raw, &js); err != nil {
		return nil, fmt.Errorf("parse accounts: %w", err)
	}
	for _, j := range js {
		a, err := UnmarshalAccount(j)
		if err != nil {
			continue // 单条损坏不影响整体
		}
		s.accounts = append(s.accounts, a)
	}
	return s, nil
}

// Save 持锁全量落盘（DPAPI 加密 + 原子写）。
func (s *Store) Save() error {
	s.mu.RLock()
	js := make([]accountJSON, 0, len(s.accounts))
	for _, a := range s.accounts {
		js = append(js, a.Marshal())
	}
	s.mu.RUnlock()
	raw, err := json.MarshalIndent(js, "", "  ")
	if err != nil {
		return err
	}
	return secrets.SealFile(s.path, raw)
}

// List 返回全部账号副本（快照语义）。
func (s *Store) List() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, len(s.accounts))
	copy(out, s.accounts)
	return out
}

// ListByProvider 按上游过滤。
func (s *Store) ListByProvider(provider string) []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Account
	for _, a := range s.accounts {
		if a.Snap().Provider == provider {
			out = append(out, a)
		}
	}
	return out
}

// Upsert 插入或替换（按 provider+uid 去重）。
func (s *Store) Upsert(a *Account) error {
	snap := a.Snap()
	if snap.UID == "" || !ValidUID(snap.UID) {
		return fmt.Errorf("invalid uid %q", snap.UID)
	}
	s.mu.Lock()
	replaced := false
	for i, e := range s.accounts {
		es := e.Snap()
		if es.Provider == snap.Provider && es.UID == snap.UID {
			s.accounts[i] = a
			replaced = true
			break
		}
	}
	if !replaced {
		s.accounts = append(s.accounts, a)
	}
	s.mu.Unlock()
	return s.Save()
}

// Remove 删除账号。
func (s *Store) Remove(provider, uid string) error {
	s.mu.Lock()
	kept := s.accounts[:0]
	for _, a := range s.accounts {
		s2 := a.Snap()
		if s2.Provider == provider && s2.UID == uid {
			continue
		}
		kept = append(kept, a)
	}
	s.accounts = kept
	s.mu.Unlock()
	return s.Save()
}

// ---------------------------------------------------------------------------
// 旧版导入（workbuddy2api 的 workbuddy-*.json）
// ---------------------------------------------------------------------------

// ImportLegacyFile 解析旧版明文 auth 文件并导入。返回导入的账号。
func (s *Store) ImportLegacyFile(path string) (*Account, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a, err := ParseLegacy(raw)
	if err != nil {
		return nil, err
	}
	if err := s.Upsert(a); err != nil {
		return nil, err
	}
	return a, nil
}

// ParseLegacy 兼容旧版磁盘形态（嵌套 {auth,account} / 扁平）。
func ParseLegacy(raw []byte) (*Account, error) {
	provider := ProviderWorkBuddy
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("legacy parse: %w", err)
	}
	var j accountJSON
	j.Provider = provider
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("legacy parse: %w", err)
		}
		j.AccessToken = n.Auth.AccessToken
		j.RefreshToken = n.Auth.RefreshToken
		j.ExpiresAt = n.Auth.ExpiresAt
		j.Domain = n.Auth.Domain
		j.UID = n.Account.UID
		j.EnterpriseID = n.Account.EnterpriseID
		j.Nickname = n.Account.Nickname
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("legacy parse: %w", err)
		}
		j.AccessToken = f.AccessToken
		j.RefreshToken = f.RefreshToken
		j.ExpiresAt = f.ExpiresAt
		j.Domain = f.Domain
		j.UID = f.UID
		j.EnterpriseID = f.EnterpriseID
		j.Nickname = f.Nickname
	}
	return UnmarshalAccount(j)
}
