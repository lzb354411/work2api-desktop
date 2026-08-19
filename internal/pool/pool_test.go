// pool_test.go 积分保留阈值（CreditFloor）挑号行为单测。
package pool

import (
	"testing"

	"work2api-desktop/internal/auth"
)

func mkAccount(provider, uid string) *auth.Account {
	return &auth.Account{Provider: provider, UID: uid, AccessToken: "t"}
}

func TestPickCreditFloor(t *testing.T) {
	p := New()
	p.Sync([]*auth.Account{
		mkAccount(auth.ProviderTrae, "u1"),
		mkAccount(auth.ProviderTrae, "u2"),
		mkAccount(auth.ProviderWorkBuddy, "w1"),
	})
	p.SetCredits(auth.ProviderTrae, "u1", 50)
	p.SetCredits(auth.ProviderTrae, "u2", 500)
	p.SetCredits(auth.ProviderWorkBuddy, "w1", 80)

	// 无阈值：全局挑积分最高者
	if a := p.Pick("auto", nil, nil); a == nil || a.Snap().UID != "u2" {
		t.Fatalf("no floor: want u2, got %v", a)
	}

	// 阈值 100：u1(50)、w1(80) 被跳过，只剩 u2
	p.SetCreditFloor(100)
	if a := p.Pick("auto", nil, nil); a == nil || a.Snap().UID != "u2" {
		t.Fatalf("floor=100 auto: want u2, got %v", a)
	}
	if a := p.Pick(auth.ProviderTrae, nil, nil); a == nil || a.Snap().UID != "u2" {
		t.Fatalf("floor=100 trae: want u2, got %v", a)
	}
	// workbuddy 全部低于阈值 → 无可选
	if a := p.Pick(auth.ProviderWorkBuddy, nil, nil); a != nil {
		t.Fatalf("floor=100 workbuddy: want nil, got %s", a.Snap().UID)
	}
	// 只读路径（模型列表查询）不受阈值限制
	if a := p.PickForRead(auth.ProviderWorkBuddy); a == nil || a.Snap().UID != "w1" {
		t.Fatalf("PickForRead workbuddy: want w1, got %v", a)
	}
	// 余额等于阈值也算到线（<=）→ 暂停
	p.SetCredits(auth.ProviderTrae, "u1", 100)
	if a := p.Pick(auth.ProviderTrae, nil, nil); a == nil || a.Snap().UID != "u2" {
		t.Fatalf("floor=100 equal: want u2, got %v", a)
	}
	// 积分回填超过阈值 → 自动恢复可用
	p.SetCredits(auth.ProviderWorkBuddy, "w1", 101)
	if a := p.Pick(auth.ProviderWorkBuddy, nil, nil); a == nil || a.Snap().UID != "w1" {
		t.Fatalf("recovered: want w1, got %v", a)
	}
	// 关闭阈值（0）→ 全部恢复可选
	p.SetCreditFloor(0)
	if a := p.Pick(auth.ProviderWorkBuddy, nil, nil); a == nil || a.Snap().UID != "w1" {
		t.Fatalf("floor off: want w1, got %v", a)
	}
	if p.CreditFloor() != 0 {
		t.Fatalf("CreditFloor: want 0, got %d", p.CreditFloor())
	}
}

func TestSetCreditFloorNegative(t *testing.T) {
	p := New()
	p.SetCreditFloor(-5)
	if p.CreditFloor() != 0 {
		t.Fatalf("negative floor should clamp to 0, got %d", p.CreditFloor())
	}
}
