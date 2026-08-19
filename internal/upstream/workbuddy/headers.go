// headers.go WorkBuddy 四类请求头（common / chat / billing / refresh）。
// 全部从 Snapshot 构造；安全红线：chat 请求绝不携带 X-Refresh-Token。
package workbuddy

import (
	"net/http"

	"work2api-desktop/internal/auth"
)

func originRefererFor(region string) string {
	if region == "global" {
		return OriginRefererGlobal
	}
	return OriginRefererCN
}

// CommonHeaders 所有 API 共享的请求头。
func CommonHeaders(req *http.Request, region string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(region)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", ClientUA)
}

// ChatHeaders 在 common 之上加 chat 专属的账号头（缺省字段用 X-No-* 约定）。
func ChatHeaders(req *http.Request, s auth.Snapshot) {
	region := "cn"
	if s.Provider == auth.ProviderWorkBuddy && isGlobalDomain(s.Domain) {
		region = "global"
	}
	CommonHeaders(req, region)
	if s.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if s.UID != "" {
		req.Header.Set("X-User-Id", s.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if s.Domain != "" {
		req.Header.Set("X-Domain", s.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
}

// BillingHeaders billing 接口请求头。
func BillingHeaders(req *http.Request, s auth.Snapshot) {
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if s.UID != "" {
		req.Header.Set("X-User-Id", s.UID)
	}
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
		req.Header.Set("X-Tenant-Id", s.EnterpriseID)
	}
	if s.Domain != "" {
		req.Header.Set("X-Domain", s.Domain)
	}
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func RefreshHeaders(req *http.Request, s auth.Snapshot) {
	region := "cn"
	if isGlobalDomain(s.Domain) {
		region = "global"
	}
	CommonHeaders(req, region)
	req.Header.Set("X-Refresh-Token", s.RefreshToken)
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}

func isGlobalDomain(domain string) bool {
	return domain == "workbuddy.ai" ||
		(len(domain) > len(".workbuddy.ai") && domain[len(domain)-len(".workbuddy.ai"):] == ".workbuddy.ai")
}
