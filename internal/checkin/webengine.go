// Package checkin Trae 网页签到引擎（纯 Go + chromedp 驱动系统 Edge）。
//
// 背景：桌面客户端 claim/status 走 ByteDance 原生 TTNet 签名，第三方纯 HTTP
// 复现不了 → 恒 9074。但 work.trae.cn 网页端在真实浏览器里跑，网页端自身用
// 普通 fetch 调签到接口（实测已确认），故本引擎复用网页端的真实浏览器会话
// 来签到，天然带网页端所需的 cookie/会话，绕开 TTNet。
//
// 与现有 OAuth 账号体系的关系：本引擎使用【独立的隔离浏览器 profile】（cookie），
// 与 accounts.dat 里的 OAuth 令牌互不相关。OAuth 令牌继续服务于对话/代理功能；
// 本引擎只接管 Trae 的「签到」动作。
//
// 凭据隔离：每个网页账号一个独立 profile 目录（base/trae-web/profiles/accN），
// 不复用用户日常上网的 Edge 个人 profile，避免污染真实 cookie。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chromedp/chromedp"
)

// LogFunc 进度日志（与 Core.logf 同签名：level + format）。
type LogFunc func(level, format string, args ...any)

// Result 签到结果分类。
type Result struct {
	State  string // need_login | success | busy | not_found | unknown | error
	Detail string
}

// Status 单个网页账号的持久状态。
type Status struct {
	LoggedIn   bool   `json:"loggedIn"`   // profile 是否已登录（首次登录成功后置 true）
	LastCheckin string `json:"lastCheckin"` // 最近成功签到日期 YYYY-MM-DD
}

// edgeCandidates 系统 Edge 候选路径（Win10/11 内置，按常见位置探测）。
var edgeCandidates = []string{
	`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files (x86)\Microsoft\Edge Beta\Application\msedge.exe`,
	`C:\Program Files\Microsoft\Edge Beta\Application\msedge.exe`,
}

// DetectEdge 查找系统 Edge 可执行文件。
func DetectEdge() (string, error) {
	for _, c := range edgeCandidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("未找到系统 Edge 浏览器，请确认已安装 Microsoft Edge")
}

// ProfileDir 返回第 index 个网页账号（1 起）的隔离 profile 目录。
func ProfileDir(base string, index int) string {
	return filepath.Join(base, "trae-web", "profiles", fmt.Sprintf("acc%d", index))
}

func statusFile(base string, index int) string {
	return filepath.Join(ProfileDir(base, index), "traeweb_status.json")
}

// ReadStatus 读取网页账号状态（不存在时返回零值，不报错）。
func ReadStatus(base string, index int) Status {
	var s Status
	raw, err := os.ReadFile(statusFile(base, index))
	if err != nil {
		return s
	}
	_ = json.Unmarshal(raw, &s)
	return s
}

func writeStatus(base string, index int, s Status) {
	raw, _ := json.MarshalIndent(s, "", "  ")
	_ = os.MkdirAll(ProfileDir(base, index), 0o700)
	_ = os.WriteFile(statusFile(base, index), raw, 0o600)
}

// allocOpts 构造 Edge 启动参数。
func allocOpts(edge, profile string, headless bool) []chromedp.ExecAllocatorOption {
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(edge),
		chromedp.UserDataDir(profile),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	}
	if headless {
		// 新版 headless（Edge 支持）
		opts = append(opts, chromedp.Flag("headless", "new"))
	}
	return opts
}

// jsIsLoggedIn 判定页面是否已登录（移植自 checkin.js）。写成 IIFE 供 chromedp.Evaluate 直接求值。
const jsIsLoggedIn = `(() => {
  const txt = document.body ? document.body.innerText : '';
  const phone = document.querySelector('input[type="tel"], input[placeholder*="手机"]');
  const need = !!phone || /登录|手机号|验证码/.test(txt);
  const hasSign = /签到/.test(txt);
  return !need && (hasSign || /头像|个人中心|退出登录/.test(txt));
})()`

// jsClickSign 点击首个含「签到」的可点击元素，返回是否点到。
const jsClickSign = `(() => {
  const els = [...document.querySelectorAll('button,a,div,span,[role=button]')];
  for (const e of els) {
    const t = (e.textContent || '').trim();
    if (t && /签到/.test(t) && e.childElementCount <= 2) { e.click(); return true; }
  }
  return false;
})()`

// jsClickMenu 点击头像/个人中心/我的，返回是否点到。
const jsClickMenu = `(() => {
  const els = [...document.querySelectorAll('button,a,div,span,[role=button]')];
  for (const e of els) {
    const t = (e.textContent || '').trim();
    if (t && /头像|个人中心|我的/.test(t) && e.childElementCount <= 2) { e.click(); return true; }
  }
  return false;
})()`

// jsReadResult 读取签到结果，返回 JSON 字符串。
const jsReadResult = `(() => {
  const txt = document.body ? document.body.innerText : '';
  const s = txt.replace(/\s+/g, ' ');
  if (/已签到|签到成功|今日已签|连续签到/.test(txt)) return '{"state":"success","detail":"' + s.slice(0,120) + '"}';
  if (/操作太频繁|参与用户太多|9074/.test(txt)) return '{"state":"busy","detail":"9074 参与用户太多"}';
  return '{"state":"unknown","detail":"' + s.slice(0,120) + '"}';
})()`

// openBrowser 启动一个 Edge 上下文（headless 控制是否隐藏窗口）。
func openBrowser(edge, profile string, headless bool) (context.Context, context.CancelFunc, context.CancelFunc) {
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts(edge, profile, headless)...)
	ctx, cancel := chromedp.NewContext(allocCtx)
	return ctx, cancelAlloc, cancel
}

// Checkin 对第 index 个网页账号执行签到（无头每日模式）。
// 返回 Result；need_login 表示 profile 尚未登录，需先调用 FirstLogin。
func Checkin(base string, index int, logf LogFunc) (*Result, error) {
	if logf == nil {
		logf = func(string, string, ...any) {}
	}
	edge, err := DetectEdge()
	if err != nil {
		return nil, err
	}
	profile := ProfileDir(base, index)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, err
	}

	ctx, cancelAlloc, cancel := openBrowser(edge, profile, true)
	defer cancelAlloc()
	defer cancel()
	ctx, cancelTO := context.WithTimeout(ctx, 90*time.Second)
	defer cancelTO()

	logf("info", "trae web acc%d: 打开 Edge(无头) 访问 work.trae.cn", index)
	if err := chromedp.Run(ctx,
		chromedp.Navigate("https://work.trae.cn/"),
		chromedp.Sleep(3*time.Second),
	); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}

	loggedIn, err := evalBool(ctx, jsIsLoggedIn)
	if err != nil {
		return nil, fmt.Errorf("is_logged_in: %w", err)
	}
	if !loggedIn {
		writeStatus(base, index, Status{LoggedIn: false, LastCheckin: ReadStatus(base, index).LastCheckin})
		return &Result{State: "need_login", Detail: "该账号尚未登录，请先用「网页登录」完成手机+验证码登录"}, nil
	}

	clicked, err := evalBool(ctx, jsClickSign)
	if err != nil {
		return nil, fmt.Errorf("click_sign: %w", err)
	}
	if !clicked {
		// 尝试先点头像/个人中心，再点签到
		if m, _ := evalBool(ctx, jsClickMenu); m {
			chromedp.Run(ctx, chromedp.Sleep(1500*time.Millisecond))
			clicked, _ = evalBool(ctx, jsClickSign)
		}
	}
	if !clicked {
		writeStatus(base, index, Status{LoggedIn: true, LastCheckin: ReadStatus(base, index).LastCheckin})
		return &Result{State: "not_found", Detail: "页面未找到签到入口，可在脚本补充选择器"}, nil
	}

	chromedp.Run(ctx, chromedp.Sleep(3*time.Second))
	raw, err := evalString(ctx, jsReadResult)
	if err != nil {
		return nil, fmt.Errorf("read_result: %w", err)
	}
	var r Result
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("parse_result: %w", err)
	}
	if r.State == "success" {
		writeStatus(base, index, Status{LoggedIn: true, LastCheckin: todayStr()})
	}
	logf("info", "trae web acc%d: 签到结果 %s", index, r.Detail)
	return &r, nil
}

// FirstLogin 启动有头 Edge 让用户用手机+验证码登录，轮询直到登录成功或超时。
// 成功后会话被持久化到 profile，后续 Checkin 无需再输入。
func FirstLogin(base string, index int, timeout time.Duration, logf LogFunc) error {
	if logf == nil {
		logf = func(string, string, ...any) {}
	}
	edge, err := DetectEdge()
	if err != nil {
		return err
	}
	profile := ProfileDir(base, index)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return err
	}

	ctx, cancelAlloc, cancel := openBrowser(edge, profile, false) // 有头：用户可见可操作
	defer cancelAlloc()
	defer cancel()
	ctx, cancelTO := context.WithTimeout(ctx, timeout)
	defer cancelTO()

	logf("info", "trae web acc%d: 打开 Edge(有头) 等待手机+验证码登录（上限 %s）", index, timeout.Round(time.Second))
	if err := chromedp.Run(ctx, chromedp.Navigate("https://work.trae.cn/")); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		loggedIn, err := evalBool(ctx, jsIsLoggedIn)
		if err == nil && loggedIn {
			writeStatus(base, index, Status{LoggedIn: true, LastCheckin: ReadStatus(base, index).LastCheckin})
			logf("info", "trae web acc%d: 登录成功，会话已保存", index)
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("登录超时（%s），请重试或重新发起网页登录", timeout.Round(time.Second))
}

func evalBool(ctx context.Context, expr string) (bool, error) {
	var v bool
	err := chromedp.Run(ctx, chromedp.Evaluate(expr, &v))
	return v, err
}

func evalString(ctx context.Context, expr string) (string, error) {
	var v string
	err := chromedp.Run(ctx, chromedp.Evaluate(expr, &v))
	return v, err
}

func todayStr() string {
	t := time.Now()
	return fmt.Sprintf("%04d-%02d-%02d", t.Year(), t.Month(), t.Day())
}
