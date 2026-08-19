// Package server OpenAI 兼容 HTTP 接口：/v1/models + /v1/chat/completions + /status。
//
// 路由规则：模型名带 trae/ 前缀走 TRAE、wb/ 走 WorkBuddy（前缀剥离后透传）；
// 无前缀按配置的默认上游（trae|workbuddy|auto）。
//
// 安全加固（相对原版）：
//   - 默认仅监听 127.0.0.1（由 app 层控制，本包不开放 0.0.0.0 默认值）
//   - API Key 必填校验（空 Key 直接 503 拒绝服务，杜绝"默认空鉴权"）
//   - 请求体 8MB 上限；错误响应不透传上游 body 原文（脱敏）
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"work2api-desktop/internal/auth"
	"work2api-desktop/internal/pool"
	"work2api-desktop/internal/upstream/trae"
	"work2api-desktop/internal/upstream/workbuddy"
)

// Deps 依赖注入（config 走闭包实现动态读取）。
type Deps struct {
	Pool            *pool.Pool
	Store           *auth.Store
	Trae            *trae.Client
	WB              *workbuddy.Client
	APIKey          func() string
	DefaultProvider func() string            // trae | workbuddy | auto
	DefaultModel    func(provider string) string // 各上游默认模型（空模型名请求回退；nil 用内置回退）
	ProviderEnabled func(provider string) bool  // 上游启用状态（UI 开关；nil = 全部启用）
	Logf            func(format string, args ...any)
}

// providerEnabled 某上游是否启用（nil 闭包 = 全部启用）。
func (h *Handler) providerEnabled(provider string) bool {
	if h.d.ProviderEnabled == nil {
		return true
	}
	return h.d.ProviderEnabled(provider)
}

// 轮转与冷却参数。
const (
	maxRotate      = 3
	hardCooldown   = 12 * time.Hour
	softCooldown   = 60 * time.Second
	errThreshold   = 3
	errCooldown    = 10 * time.Minute
	refreshSkew    = 10 * time.Minute
	maxBodyBytes   = 8 << 20
	modelsTTL      = time.Hour
	modelsFailCool = 5 * time.Minute
)

// Handler 主路由。
type Handler struct {
	d   Deps
	mux *http.ServeMux

	modelsMu     sync.Mutex
	modelsCache  map[string][]modelEntry // provider → 列表
	modelsFetched time.Time
	modelsFail   time.Time
}

type modelEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	Name          string `json:"name,omitempty"` // 展示名（动态拉取时有）
	ContextLength int64  `json:"context_length"`
	MaxTokens     int64  `json:"max_output_tokens"`
}

// ModelBrief 模型简要信息（UI 模型页展示用）。
type ModelBrief struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Context   int64  `json:"contextLength"`
	MaxTokens int64  `json:"maxTokens"`
}

// ListProviderModels 指定上游的模型列表（动态优先，静态兜底）。provider: trae|workbuddy。
func (h *Handler) ListProviderModels(provider string) []ModelBrief {
	var fallback []modelEntry
	switch provider {
	case auth.ProviderTrae:
		fallback = staticTraeModels
	case auth.ProviderWorkBuddy:
		fallback = staticWBModels
	default:
		return []ModelBrief{}
	}
	entries := h.providerModels(provider, fallback)
	out := make([]ModelBrief, 0, len(entries))
	for _, e := range entries {
		out = append(out, ModelBrief{ID: e.ID, Name: e.Name, Context: e.ContextLength, MaxTokens: e.MaxTokens})
	}
	return out
}

// RefreshModels 清空模型缓存并重新拉取两个上游（UI 手动刷新用）。
func (h *Handler) RefreshModels() {
	h.modelsMu.Lock()
	h.modelsCache = map[string][]modelEntry{}
	h.modelsFetched = time.Time{}
	h.modelsFail = time.Time{}
	h.modelsMu.Unlock()
	h.providerModels(auth.ProviderTrae, staticTraeModels)
	h.providerModels(auth.ProviderWorkBuddy, staticWBModels)
}

// New 构建 handler。
func New(d Deps) *Handler {
	h := &Handler{
		d:           d,
		mux:         http.NewServeMux(),
		modelsCache: map[string][]modelEntry{},
	}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.d.Pool.List()})
}

// withAuth Bearer Key 校验。空 Key = 尚未初始化完成 → 直接拒绝。
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := h.d.APIKey()
		if key == "" {
			writeOpenAIError(w, http.StatusServiceUnavailable, "server_not_ready", "api key not initialized")
			return
		}
		authz := r.Header.Get("Authorization")
		if authz == "" {
			authz = "Bearer " + r.URL.Query().Get("key") // 兼容部分 harness 的 query 传参
		}
		if !secureEqualBearer(authz, key) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// secureEqualBearer 常数时间比较，避免时序侧信道。
func secureEqualBearer(authz, key string) bool {
	const prefix = "Bearer "
	if len(authz) != len(prefix)+len(key) || !strings.HasPrefix(authz, prefix) {
		return false
	}
	got := authz[len(prefix):]
	var v byte
	for i := 0; i < len(key); i++ {
		v |= key[i] ^ got[i]
	}
	return v == 0
}

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

// 静态回退模型表（动态接口失败时兜底）。
// 数据取自 2026-08 实测：TRAE get_detail_param 可见模型（过滤 is_invisible_to_user），
// WorkBuddy console/models 的 cli 交集。
var staticTraeModels = []modelEntry{
	{ID: "DeepSeek-V4-Flash-Official", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "DeepSeek-V4-Flash 正式版", ContextLength: 200000, MaxTokens: 32000},
	{ID: "DeepSeek-V4-Pro-Official", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "DeepSeek-V4-Pro 正式版", ContextLength: 200000, MaxTokens: 32000},
	{ID: "DeepSeek-V4-Flash", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "DeepSeek-V4-Flash", ContextLength: 200000, MaxTokens: 32000},
	{ID: "DeepSeek-V4-Pro", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "DeepSeek-V4-Pro", ContextLength: 200000, MaxTokens: 32000},
	{ID: "deepseek-v4-flash", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "DeepSeek-V4-Flash", ContextLength: 200000, MaxTokens: 16000},
	{ID: "glm-5.2", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "GLM-5.2", ContextLength: 200000, MaxTokens: 32000},
	{ID: "glm-5", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "GLM-5", ContextLength: 200000, MaxTokens: 32000},
	{ID: "kimi-k3", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "Kimi-K3", ContextLength: 200000, MaxTokens: 32000},
	{ID: "kimi-k2.7-code", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "Kimi-K2.7-Code", ContextLength: 200000, MaxTokens: 32000},
	{ID: "kimi-k2.6", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "Kimi-K2.6", ContextLength: 200000, MaxTokens: 32000},
	{ID: "minimax-m3", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "MiniMax-M3", ContextLength: 200000, MaxTokens: 32000},
	{ID: "qwen3.8-max", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "Qwen3.8-Max", ContextLength: 200000, MaxTokens: 32000},
	{ID: "qwen-3.7-plus", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "Qwen3.7-Plus", ContextLength: 200000, MaxTokens: 32000},
	{ID: "Doubao-Seed-2.1-Pro", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "Seed-2.1-Pro", ContextLength: 232768, MaxTokens: 32768},
	{ID: "Doubao-Seed-2.1-Turbo", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "Seed-2.1-Turbo", ContextLength: 232768, MaxTokens: 32768},
	{ID: "custom_model_gemini", Object: "model", Created: 1753600000, OwnedBy: "trae", Name: "Gemini-3.1-Pro-Preview", ContextLength: 300000, MaxTokens: 32000},
}

var staticWBModels = []modelEntry{
	{ID: "deepseek-v4-flash", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "Deepseek-V4-Flash", ContextLength: 1000000, MaxTokens: 50000},
	{ID: "deepseek-v4-pro", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "Deepseek-V4-Pro", ContextLength: 1000000, MaxTokens: 50000},
	{ID: "glm-5.3", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "GLM-5.3", ContextLength: 1000000, MaxTokens: 48000},
	{ID: "glm-5.2", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "GLM-5.2", ContextLength: 1000000, MaxTokens: 48000},
	{ID: "glm-5.1", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "GLM-5.1", ContextLength: 200000, MaxTokens: 48000},
	{ID: "glm-5v-turbo", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "GLM-5v-Turbo", ContextLength: 200000, MaxTokens: 64000},
	{ID: "kimi-k3-1", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "Kimi-K3", ContextLength: 1000000, MaxTokens: 32000},
	{ID: "kimi-k2.7", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "Kimi-K2.7-Code", ContextLength: 256000, MaxTokens: 32000},
	{ID: "kimi-k2.6", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "Kimi-K2.6", ContextLength: 256000, MaxTokens: 32000},
	{ID: "minimax-m3", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "MiniMax-M3", ContextLength: 512000, MaxTokens: 128000},
	{ID: "hy3", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "Hy3", ContextLength: 192000, MaxTokens: 64000},
	{ID: "auto", Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: "Auto", ContextLength: 168000, MaxTokens: 32000},
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	out := make([]modelEntry, 0, 16)
	out = append(out, h.providerModels(auth.ProviderTrae, staticTraeModels)...)
	out = append(out, h.providerModels(auth.ProviderWorkBuddy, staticWBModels)...)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// providerModels 单上游模型列表：动态优先（1h 缓存 + 5min 失败负缓存），失败回退静态。
func (h *Handler) providerModels(provider string, fallback []modelEntry) []modelEntry {
	h.modelsMu.Lock()
	cached, ok := h.modelsCache[provider]
	fresh := !h.modelsFetched.IsZero() && time.Since(h.modelsFetched) < modelsTTL
	failCool := !h.modelsFail.IsZero() && time.Since(h.modelsFail) < modelsFailCool
	h.modelsMu.Unlock()
	if ok && fresh {
		return cached
	}
	if failCool {
		if ok {
			return cached
		}
		return fallback
	}

	// 动态拉取：任一该上游健康账号（只读挑号：不受启用开关与积分保留阈值限制，
	// 关闭上游或账号低于阈值后仍可浏览其模型表并选择默认模型）
	var infos any
	var err error
	acct := h.d.Pool.PickForRead(provider)
	if acct == nil {
		return fallback
	}
	switch provider {
	case auth.ProviderTrae:
		var m []trae.ModelInfo
		m, err = h.d.Trae.FetchModels(acct)
		infos = m
	case auth.ProviderWorkBuddy:
		var m []workbuddy.ModelInfo
		m, err = h.d.WB.FetchModels(acct)
		infos = m
	}
	if err != nil {
		h.modelsMu.Lock()
		h.modelsFail = time.Now()
		h.modelsMu.Unlock()
		if ok {
			return cached
		}
		return fallback
	}

	entries := make([]modelEntry, 0)
	switch v := infos.(type) {
	case []trae.ModelInfo:
		for _, m := range v {
			entries = append(entries, modelEntry{ID: m.ID, Object: "model", Created: 1753600000, OwnedBy: "trae", Name: m.Name, ContextLength: m.ContextWindow, MaxTokens: m.MaxTokens})
		}
	case []workbuddy.ModelInfo:
		for _, m := range v {
			// 上下文取上游 maxInputTokens（FetchModels 已解析）；0 = 上游未返回，前端显示为未知
			entries = append(entries, modelEntry{ID: m.ID, Object: "model", Created: 1753600000, OwnedBy: "workbuddy", Name: m.Name, ContextLength: m.ContextWindow, MaxTokens: m.MaxTokens})
		}
	}
	if len(entries) == 0 {
		return fallback
	}
	h.modelsMu.Lock()
	h.modelsCache[provider] = entries
	h.modelsFetched = time.Now()
	h.modelsFail = time.Time{}
	h.modelsMu.Unlock()
	return entries
}

// ---------------------------------------------------------------------------
// chat completions
// ---------------------------------------------------------------------------

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body failed")
		return
	}
	if len(body) == maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 8MB limit")
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	provider, cleanModel := routeModel(peek.Model, h.d.DefaultProvider())
	if cleanModel != peek.Model {
		// 剥离路由前缀后重写 model 字段
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			obj["model"] = cleanModel
			if nb, err := json.Marshal(obj); err == nil {
				body = nb
			}
		}
	}

	// 未指定模型（空模型名）→ 按实际路由到的上游补各自默认模型（DeepSeek v4 flash 正式版）。
	// auto 模式下同一请求可能轮换到不同上游，为两套上游各准备一份 body。
	var traeBody, wbBody []byte
	if cleanModel == "" {
		traeBody = withModel(body, h.modelDefault(auth.ProviderTrae))
		wbBody = withModel(body, h.modelDefault(auth.ProviderWorkBuddy))
	}

	// 上游启用开关：显式路由（前缀/默认上游）到被禁用上游 → 403；
	// auto 模式只从启用上游中挑号，不消耗被禁用上游的积分。
	if provider != "auto" && !h.providerEnabled(provider) {
		writeOpenAIError(w, http.StatusForbidden, "provider_disabled",
			"upstream '"+provider+"' is disabled (enable it in the Models page)")
		return
	}
	var skip map[string]bool
	if provider == "auto" {
		if !h.providerEnabled(auth.ProviderTrae) || !h.providerEnabled(auth.ProviderWorkBuddy) {
			skip = map[string]bool{}
			if !h.providerEnabled(auth.ProviderTrae) {
				skip[auth.ProviderTrae] = true
			}
			if !h.providerEnabled(auth.ProviderWorkBuddy) {
				skip[auth.ProviderWorkBuddy] = true
			}
		}
	}

	tried := map[pool.Key]bool{}
	var lastMsg string
	for i := 0; i < maxRotate; i++ {
		acct := h.d.Pool.Pick(provider, tried, skip)
		if acct == nil {
			break
		}
		s := acct.Snap()
		tried[pool.Key{Provider: s.Provider, UID: s.UID}] = true

		// token 临近过期 → 先刷新（失败冷却/禁用换号）
		if acct.NeedsRefresh(refreshSkew) {
			var rerr error
			if s.Provider == auth.ProviderTrae {
				rerr = h.d.Trae.RefreshToken(acct)
			} else {
				rerr = h.d.WB.RefreshToken(acct)
			}
			if rerr != nil {
				h.d.Pool.Cooldown(s.Provider, s.UID, errCooldown, "refresh failed")
				lastMsg = desensitize(rerr.Error())
				continue
			}
			_ = h.d.Store.Save()
			s = acct.Snap()
		}

		if s.Provider == auth.ProviderTrae {
			b := body
			if traeBody != nil {
				b = traeBody
			}
			done := h.chatViaTrae(w, acct, b, peek.Stream)
			if done {
				return
			}
			continue
		}
		b := body
		if wbBody != nil {
			b = wbBody
		}
		if h.chatViaWorkBuddy(w, acct, b, peek.Stream) {
			return
		}
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if floor := h.d.Pool.CreditFloor(); floor > 0 {
		msg += fmt.Sprintf("/credit floor %d", floor)
	}
	if lastMsg != "" {
		msg += ": " + lastMsg
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// routeModel 模型名 → (provider, cleanModel)。前缀路由 > 默认配置。
func routeModel(model, defaultProvider string) (provider, clean string) {
	clean = strings.TrimSpace(model)
	switch {
	case strings.HasPrefix(clean, "trae/"):
		return auth.ProviderTrae, strings.TrimPrefix(clean, "trae/")
	case strings.HasPrefix(clean, "wb/"):
		return auth.ProviderWorkBuddy, strings.TrimPrefix(clean, "wb/")
	}
	if defaultProvider == "" || defaultProvider == "auto" {
		return "auto", clean
	}
	return defaultProvider, clean
}

// fallbackDefaultModels 内置回退默认模型（各上游 DeepSeek v4 flash 正式版，实测 ID）。
var fallbackDefaultModels = map[string]string{
	auth.ProviderTrae:      "DeepSeek-V4-Flash-Official",
	auth.ProviderWorkBuddy: "deepseek-v4-flash",
}

// modelDefault 某上游的默认模型：配置优先，内置 DeepSeek v4 flash 正式版兜底。
func (h *Handler) modelDefault(provider string) string {
	if h.d.DefaultModel != nil {
		if m := h.d.DefaultModel(provider); m != "" {
			return m
		}
	}
	return fallbackDefaultModels[provider]
}

// withModel 重写请求体的 model 字段；解析失败时返回原 body（由上游报错）。
func withModel(body []byte, model string) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	obj["model"] = model
	if nb, err := json.Marshal(obj); err == nil {
		return nb
	}
	return body
}

// chatViaTrae 单账号 TRAE 请求。返回 true 表示响应已写完（无论成败）；
// false 表示可轮换下一账号（已计入冷却）。
func (h *Handler) chatViaTrae(w http.ResponseWriter, acct *auth.Account, body []byte, stream bool) bool {
	s := acct.Snap()
	rc, status, respBody, err := h.d.Trae.ChatStream(acct, body)
	if err != nil {
		h.d.Pool.NoteError(s.Provider, s.UID, errThreshold, errCooldown)
		return false
	}
	if status >= 400 {
		h.applyUpstreamError(s.Provider, s.UID, trae.Classify(status, string(respBody)), "upstream")
		return false
	}
	defer rc.Close()
	h.d.Pool.NoteSuccess(s.Provider, s.UID)
	if stream {
		_ = trae.StreamWithError(w, rc, func(se *trae.SOLOStreamError) {
			if se.Kind() == trae.ErrPlanLimit {
				h.d.Pool.Cooldown(s.Provider, s.UID, hardCooldown, "plan limit")
			}
		})
		return true
	}
	resp, aerr := trae.Aggregate(rc)
	if aerr != nil {
		var se *trae.SOLOStreamError
		if errors.As(aerr, &se) && se.Kind() == trae.ErrPlanLimit {
			h.d.Pool.Cooldown(s.Provider, s.UID, hardCooldown, "plan limit")
			writeOpenAIError(w, http.StatusBadGateway, "plan_limit", "upstream plan limit reached")
			return true
		}
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "upstream stream parse failed")
		return true
	}
	writeJSON(w, http.StatusOK, resp)
	return true
}

// chatViaWorkBuddy 单账号 WorkBuddy 请求。语义同 chatViaTrae。
func (h *Handler) chatViaWorkBuddy(w http.ResponseWriter, acct *auth.Account, body []byte, stream bool) bool {
	s := acct.Snap()
	rc, status, respBody, err := h.d.WB.ChatStream(acct, body)
	if err != nil {
		h.d.Pool.NoteError(s.Provider, s.UID, errThreshold, errCooldown)
		return false
	}
	if status >= 400 {
		h.applyUpstreamError(s.Provider, s.UID, workbuddy.Classify(status, string(respBody)), "upstream")
		return false
	}
	defer rc.Close()
	h.d.Pool.NoteSuccess(s.Provider, s.UID)
	if stream {
		_ = workbuddy.Stream(w, rc)
		return true
	}
	resp, aerr := workbuddy.Aggregate(rc)
	if aerr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "upstream stream parse failed")
		return true
	}
	writeJSON(w, http.StatusOK, resp)
	return true
}

// applyUpstreamError 按错误类别冷却/禁用。
func (h *Handler) applyUpstreamError(provider, uid string, kind interface{ String() string }, source string) {
	switch fmt.Sprint(kind) {
	case "plan_limit", "hard_credit":
		h.d.Pool.Cooldown(provider, uid, hardCooldown, source+": credit/plan limit")
	case "soft_rate", "not_found":
		h.d.Pool.Cooldown(provider, uid, softCooldown, source+": rate limit")
	case "session_dead":
		h.d.Pool.Disable(provider, uid, source+": session dead")
	default:
		h.d.Pool.NoteError(provider, uid, errThreshold, errCooldown)
	}
}

// desensitize 错误信息脱敏：剥掉可能带 token 的长串。
func desensitize(s string) string {
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
