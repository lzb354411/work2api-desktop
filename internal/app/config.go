// Package app 应用核心：配置、数据目录、日志环、后台调度、API 服务生命周期。
package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"work2api-desktop/internal/secrets"
)

// Version 应用版本。
const Version = "1.3.0"

// Config 应用配置（整体 DPAPI 加密落盘为 config.dat）。
type Config struct {
	Port             int      `json:"port"`             // API 监听端口，默认 8317
	APIKey           string   `json:"apiKey"`           // 必填，首次启动自动生成
	DefaultProvider  string   `json:"defaultProvider"`  // workbuddy | auto
	CheckinEnabled   bool     `json:"checkinEnabled"`   // 每日自动签到
	CheckinTime      string   `json:"checkinTime"`      // HH:MM，默认 09:05
	StartMinimized   bool     `json:"startMinimized"`   // 启动时最小化到托盘
	AutoStart        bool     `json:"autoStart"`        // 开机自动启动（HKCU Run 注册表）
	CreditFloor      int64    `json:"creditFloor"`      // 积分保留阈值：账号余额 <= 该值时暂停挑号（0 = 不限制）
	DisabledProviders []string `json:"disabledProviders"` // 被禁用的上游（UI 开关关闭；缺省=全部启用）
}

// ProviderEnabled 上游是否启用（未列入 DisabledProviders 即启用）。
func (c *Config) ProviderEnabled(provider string) bool {
	for _, p := range c.DisabledProviders {
		if p == provider {
			return false
		}
	}
	return true
}

// LogEntry 环形日志条目。
type LogEntry struct {
	Ms    int64  `json:"ms"`    // Unix 毫秒（Wails 绑定不映射 time.Time，用数值）
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// RingLog 固定容量内存日志环（脱敏后内容，供 UI 展示）。
type RingLog struct {
	mu    sync.Mutex
	ents  []LogEntry
	cap   int
	subMu sync.Mutex
	subs  []chan LogEntry
}

// NewRingLog 构建日志环。
func NewRingLog(capacity int) *RingLog {
	return &RingLog{cap: capacity}
}

// Logf 写入一条日志。
func (r *RingLog) Logf(level, format string, args ...any) {
	e := LogEntry{Ms: time.Now().UnixMilli(), Level: level, Msg: fmt.Sprintf(format, args...)}
	r.mu.Lock()
	r.ents = append(r.ents, e)
	if len(r.ents) > r.cap {
		r.ents = r.ents[len(r.ents)-r.cap:]
	}
	r.mu.Unlock()

	r.subMu.Lock()
	for _, ch := range r.subs {
		select {
		case ch <- e:
		default: // 慢消费者丢弃
		}
	}
	r.subMu.Unlock()
}

// Snapshot 返回最近 n 条（n<=0 全量）。
func (r *RingLog) Snapshot(n int) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.ents) {
		n = len(r.ents)
	}
	out := make([]LogEntry, n)
	copy(out, r.ents[len(r.ents)-n:])
	return out
}

// Subscribe 订阅新日志流（UI Events 推送用）。
func (r *RingLog) Subscribe() chan LogEntry {
	ch := make(chan LogEntry, 64)
	r.subMu.Lock()
	r.subs = append(r.subs, ch)
	r.subMu.Unlock()
	return ch
}

// ---------------------------------------------------------------------------
// 数据目录
// ---------------------------------------------------------------------------

// DataDir 应用数据目录（%APPDATA%\work2api-desktop）。
func DataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "work2api-desktop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ---------------------------------------------------------------------------
// 配置读写（加密）
// ---------------------------------------------------------------------------

func defaultConfig() *Config {
	return &Config{
		Port:                8317,
		DefaultProvider:     "auto",
		CheckinEnabled:      true,
		CheckinTime:         "09:05",
		AutoStart:           false,
	}
}

// GenerateAPIKey 生成 32 字符 hex 随机 Key。
func GenerateAPIKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// LoadConfig 加载配置；首次启动自动生成 API Key 并落盘。
func LoadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	raw, err := openSealed(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.APIKey = GenerateAPIKey()
			if err := saveSealed(path, cfg); err != nil {
				return nil, err
			}
			return cfg, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// 安全兜底：空 Key / 非法端口 → 重新生成/修正
	if cfg.APIKey == "" {
		cfg.APIKey = GenerateAPIKey()
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		cfg.Port = 8317
	}
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = "auto"
	}
	// 积分保留阈值：负数视为未启用（0 = 不限制）
	if cfg.CreditFloor < 0 {
		cfg.CreditFloor = 0
	}
	return cfg, nil
}

// SaveConfig 保存配置（加密落盘）。
func SaveConfig(path string, cfg *Config) error {
	return saveSealed(path, cfg)
}

// ---------------------------------------------------------------------------
// sealed 文件薄封装
// ---------------------------------------------------------------------------

func openSealed(path string) ([]byte, error) { return secrets.OpenFile(path) }
func saveSealed(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return secrets.SealFile(path, raw)
}
