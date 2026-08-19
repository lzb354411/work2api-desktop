// Package workbuddy 封装 WorkBuddy（CodeBuddy CN / workbuddy.ai）上游调用。
package workbuddy

const (
	ClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	OriginRefererCN     = "https://www.codebuddy.cn"
	OriginRefererGlobal = "https://www.workbuddy.ai"

	ChatBaseCN      = "https://copilot.tencent.com"
	BillingBaseCN   = "https://www.codebuddy.cn"
	ChatBaseGlobal  = "https://www.workbuddy.ai"
	BillingBaseGlob = "https://www.workbuddy.ai"

	// 默认模型表（动态拉取失败时回退）。
	DefaultModel = "auto"
)

// staticModels 静态回退模型表。
var staticModels = []string{
	"glm-5.2", "glm-5.1", "glm-5v-turbo", "kimi-k2.7", "minimax-m3",
	"hy3", "hy3-preview", "hy3-preview-agent", "deepseek-v4-pro", "deepseek-v4-flash",
}
