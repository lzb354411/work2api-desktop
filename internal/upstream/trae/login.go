// login.go TRAE SOLO 登录：构造授权 URL + 本地回调解析。
//
// 相对原版 login.sh（bash+python）的改进：
//   - 回调由内嵌 127.0.0.1:18080 临时 HTTP 监听自动捕获（仅登录期间存在，
//     只绑 loopback），不再要求用户手工粘贴含 refreshToken 的回调链接，
//     避免敏感参数经终端/剪贴板二次泄露
//   - 回调 URL 中的 refreshToken 只进内存，落盘前经 DPAPI 加密
package trae

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
)

// LoginContext 一次 TRAE 登录的上下文（machine/device id 与回调共用）。
type LoginContext struct {
	MachineID string
	DeviceID  string
	URL       string
}

func randHex32() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 失败属于不可恢复的系统故障
	}
	return hex.EncodeToString(b)
}

// BuildLoginURL 构造登录链接（每次生成新的 machine/device id）。
func (c *Client) BuildLoginURL(callbackPort int) *LoginContext {
	machineID := randHex32()
	deviceID := randHex32()
	traceID := randHex32()[:16]

	callback := fmt.Sprintf("http://127.0.0.1:%d/authorize", callbackPort)
	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", "2.3.62834")
	q.Set("auth_type", "local")
	q.Set("client_id", c.ClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", traceID)
	q.Set("auth_callback_url", callback)
	q.Set("machine_id", machineID)
	q.Set("device_id", deviceID)
	q.Set("x_device_id", deviceID)
	q.Set("x_machine_id", machineID)
	q.Set("x_device_brand", "PC")
	q.Set("x_device_type", "PC")
	q.Set("x_os_version", "1.0")
	q.Set("x_app_version", IdeVersion)
	q.Set("x_app_type", "stable")

	return &LoginContext{
		MachineID: machineID,
		DeviceID:  deviceID,
		URL:       ConsoleHost + "/authorization?" + q.Encode(),
	}
}

// CallbackResult 回调解析产物。
type CallbackResult struct {
	RefreshToken string
	UID          string
	Nickname     string
	EnterpriseID string
}

// ParseCallback 解析回调 URL query（refreshToken / userInfo / userJwt）。
// 兼容 userInfo 缺失时的 userJwt 兜底。
func ParseCallback(q url.Values) (*CallbackResult, error) {
	res := &CallbackResult{
		RefreshToken: q.Get("refreshToken"),
	}

	parseJSONParam := func(raw string) map[string]any {
		if raw == "" {
			return nil
		}
		for _, v := range []string{raw} {
			var obj map[string]any
			if err := json.Unmarshal([]byte(v), &obj); err == nil {
				return obj
			}
			// query 解码后可能还嵌一层 URL 编码
			if dec, err := url.QueryUnescape(v); err == nil {
				if err := json.Unmarshal([]byte(dec), &obj); err == nil {
					return obj
				}
			}
		}
		return nil
	}

	userInfo := parseJSONParam(q.Get("userInfo"))
	userJwt := parseJSONParam(q.Get("userJwt"))

	if userInfo != nil {
		res.UID = str(userInfo["UserID"])
		res.Nickname = str(userInfo["ScreenName"])
		res.EnterpriseID = str(userInfo["TenantID"])
	}
	if res.RefreshToken == "" && userJwt != nil {
		res.RefreshToken = str(userJwt["RefreshToken"])
	}
	if res.RefreshToken == "" {
		return nil, fmt.Errorf("callback missing refreshToken")
	}
	return res, nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
