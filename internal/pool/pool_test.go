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
		mkAccount(auth.ProviderWorkBuddy, "w1"),
		mkAccount(auth.ProviderWorkBuddy, "w2"),
		mkAccount(auth.ProviderWorkBuddy, "w3"),
	})
	p.SetCredits(auth.ProviderWorkBuddy, "w1", 50)
	p.SetCredits(auth.ProviderWorkBuddy, "w2", 500)
	p.SetCredits(auth.ProviderWorkBuddy, "w3", 80)

	// 无阈值：全局挑积分最高者
	if a := p.Pick("auto", nil, nil); a == nil || a.Snap().UID != "w2" {
		t.Fatalf("no floor: want w2, got %v", a)
	}

	// 阈值 100：w1(50)、w3(80) 被跳过，只剩 w2
	p.SetCreditFloor(100)
	if a := p.Pick("auto", nil, nil); a == nil || a.Snap().UID != "w2" {
		t.Fatalf("floor=100 auto: want w2, got %v", a)
	}
	if a := p.Pick(auth.ProviderWorkBuddy, nil, nil); a == nil || a.Snap().UID != "w2" {
		t.Fatalf("floor=100 workbuddy: want w2, got %v", a)
	}
	// 只读路径（模型列表查询）不受阈值限制
	if a := p.PickForRead(auth.ProviderWorkBuddy); a == nil || a.Snap().UID != "w2" {
		t.Fatalf("PickForRead workbuddy: want w2, got %v", a)
	}
	// 余额等于阈值也算到线（<=）→ 暂停；w1 到线后仍应挑到 w2
	p.SetCredits(auth.ProviderWorkBuddy, "w1", 100)
	if a := p.Pick(auth.ProviderWorkBuddy, nil, nil); a == nil || a.Snap().UID != "w2" {
		t.Fatalf("floor=100 equal: want w2, got %v", a)
	}
	// 全部低于阈值 → 无可选
	p.SetCredits(auth.ProviderWorkBuddy, "w1", 50)
	p.SetCredits(auth.ProviderWorkBuddy, "w2", 90)
	if a := p.Pick(auth.ProviderWorkBuddy, nil, nil); a != nil {
		t.Fatalf("all below floor: want nil, got %s", a.Snap().UID)
	}
	// 积分回填超过阈值 → 自动恢复可用
	p.SetCredits(auth.ProviderWorkBuddy, "w3", 101)
	if a := p.Pick(auth.ProviderWorkBuddy, nil, nil); a == nil || a.Snap().UID != "w3" {
		t.Fatalf("recovered: want w3, got %v", a)
	}
	// 关闭阈值（0）→ 全部恢复可选（最高分 w3=101）
	p.SetCreditFloor(0)
	if a := p.Pick(auth.ProviderWorkBuddy, nil, nil); a == nil || a.Snap().UID != "w3" {
		t.Fatalf("floor off: want w3, got %v", a)
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