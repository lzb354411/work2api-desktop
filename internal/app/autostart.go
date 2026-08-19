// autostart.go 开机自启动管理（Windows 注册表 HKCU Run）。
//
// 安全设计：
//   - 仅写 HKCU（当前用户），不需要管理员权限
//   - 值名固定 Work2API-Desktop，开关互斥（开启写入 / 关闭删除）
//   - 写入路径为当前 exe 绝对路径（带引号，兼容空格路径）
package app

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

// autoStartValue 注册表值名。
const autoStartValue = "Work2API-Desktop"

// autoStartKey 自启动注册表键。
var autoStartKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// SetAutoStart 开启/关闭开机自启动。
// enabled=true 写入当前 exe 路径；enabled=false 删除注册表值（不存在不算错误）。
func SetAutoStart(enabled bool) error {
	if enabled {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("autostart: resolve exe: %w", err)
		}
		k, err := registry.OpenKey(registry.CURRENT_USER, autoStartKey, registry.SET_VALUE)
		if err != nil {
			return fmt.Errorf("autostart: open run key: %w", err)
		}
		defer k.Close()
		if err := k.SetStringValue(autoStartValue, `"`+exe+`"`); err != nil {
			return fmt.Errorf("autostart: set value: %w", err)
		}
		return nil
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, autoStartKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open run key: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(autoStartValue); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("autostart: delete value: %w", err)
	}
	return nil
}

// AutoStartEnabled 查询注册表自启动是否已开启。
func AutoStartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autoStartKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(autoStartValue)
	return err == nil
}
