package main

import (
	"embed"
	"log"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

// 托盘图标：多尺寸未压缩 .ico（明/暗两套），由 cmd/genicon 生成。
// 不能改成 PNG —— Wails 对 PNG 输入不缩放，1024px 直接喂进通知区域只会坏掉。
//
//go:embed build/tray/tray-lightbg.ico
var trayIconLight []byte

//go:embed build/tray/tray-darkbg.ico
var trayIconDark []byte

func init() {
	application.RegisterEvent[*TermData]("term:data")
	application.RegisterEvent[*TermStatus]("term:status")
	application.RegisterEvent[*ZmodemProgress]("zmodem:progress")
	application.RegisterEvent[*WindowModeState]("app:mode")
	application.RegisterEvent[*PreviewRequest]("preview:load")
}

func main() {
	// 日志要在第一个 log 调用之前接好，否则 NewStore 失败那条就直接丢了。
	logger, logErr := SetupRunLog()
	defer logger.Close()
	out := logTargets(logger)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(out)
	if logErr != nil {
		log.Printf("日志文件初始化失败，本次运行不落盘: %v", logErr)
	}

	store, err := NewStore()
	if err != nil {
		log.Fatalf("init config store: %v", err)
	}

	configService := NewConfigService(store)
	terminalService := NewTerminalService(store)
	appService := NewAppService(store)
	previewService := NewPreviewService()

	exe, _ := os.Executable()
	log.Printf("FTRW 启动 pid=%d exe=%s log=%s", os.Getpid(), exe, logger.Path())

	app := application.New(application.Options{
		Name:        "FTRW",
		Description: "Telnet/SSH BBS Terminal (Wails 3 rewrite)",
		Logger:      newSystemLogger(out),
		Services: []application.Service{
			application.NewService(configService),
			application.NewService(terminalService),
			application.NewService(appService),
			application.NewService(previewService),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "FTRW - BBS 终端",
		Width:            1120,
		Height:           720,
		Frameless:        true,
		BackgroundColour: application.NewRGB(24, 26, 30),
		URL:              "/",
		StartState:       application.WindowStateNormal,
	})

	appService.Bind(app, win)
	appService.SetTrayIcons(trayIconLight, trayIconDark)
	previewService.Bind(app, win, appService)

	// 跨应用热键不在这里注册：低级键盘钩子要等应用跑起来之后才能安全地往主线程投递动作，
	// 见 AppService.Start 里的 startGlobalKeys。
	// 托盘必须在 Run 之前建：New() 在应用已运行时会立即把图标加进通知区域，
	// 赶在 SetIcon 之前就只能拿到 Wails 的回退图标。
	appService.SetupTray()
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		appService.Start()
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
