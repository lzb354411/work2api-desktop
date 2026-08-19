// main.go work2api-desktop 入口：Wails 桌面窗口 + 系统托盘 + 后台 API 服务。
//
// 目标平台：Windows 10/11（DPAPI 加密存储）。
// 单文件交付：wails build 产出单个 exe，无 Bash/Docker/Python 依赖。
package main

import (
	"context"
	"embed"
	"log"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"work2api-desktop/internal/app"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.ico
var trayIcon []byte

var api *API

func main() {
	core, err := app.NewCore()
	if err != nil {
		log.Fatalf("init failed: %v", err)
	}
	if err := core.Start(); err != nil {
		log.Fatalf("start failed: %v", err)
	}
	api = NewAPI(core)

	startMinimized := core.Cfg().StartMinimized

	// 系统托盘（Windows 下可在独立 goroutine 运行消息循环）
	go systray.Run(func() { onTrayReady() }, func() {})

	err = wails.Run(&options.App{
		Title:     "Work2API Desktop",
		Width:     980,
		Height:    680,
		MinWidth:  860,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 24, G: 26, B: 32, A: 255},
		StartHidden:      startMinimized,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "work2api-desktop-3f7c9e21-singleton",
			OnSecondInstanceLaunch: func(data options.SecondInstanceData) {
				if api != nil && api.ctx != nil {
					runtime.WindowShow(api.ctx)
					runtime.WindowUnminimise(api.ctx)
				}
			},
		},
		OnStartup: func(ctx context.Context) {
			api.ctx = ctx
			// 日志推送：订阅环形日志 → 前端 Events
			go func() {
				ch := core.Log.Subscribe()
				for e := range ch {
					runtime.EventsEmit(ctx, "log", e)
				}
			}()
		},
		OnBeforeClose: func(ctx context.Context) bool {
			// 关闭窗口 → 隐藏到托盘（后台继续提供 API 服务）
			runtime.WindowHide(ctx)
			return true
		},
		Bind: []interface{}{api},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			Theme:                windows.Dark,
		},
	})
	if err != nil {
		log.Fatalf("wails run failed: %v", err)
	}
	core.Stop()
}

func onTrayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("Work2API")
	systray.SetTooltip("Work2API Desktop — OpenAI 兼容代理")
	systray.SetOnTapped(func() { showMainWindow() })

	mShow := systray.AddMenuItem("打开主界面", "显示主窗口")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出应用（API 服务停止）")

	go func() {
		for {
			select {
			case <-mShow.ClickedCh:
				showMainWindow()
			case <-mQuit.ClickedCh:
				if api != nil && api.ctx != nil {
					runtime.Quit(api.ctx)
				}
				systray.Quit()
				return
			}
		}
	}()
}

func showMainWindow() {
	if api != nil && api.ctx != nil {
		runtime.WindowShow(api.ctx)
		runtime.WindowUnminimise(api.ctx)
	}
}
