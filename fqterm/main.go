package main

import (
	"embed"
	"log"
	"os"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// browserArgsFromEnv 把 FTRW_BROWSER_ARGS 里的空格分隔参数交给 WebView2（只在环境变量
// 设了的时候才起作用，平时零影响）。常用一条：--remote-debugging-port=9444，然后用
// CDP 在真进程里看运行时数据（页内钩子是前端 App.vue 的 window.__ftrw）。
func browserArgsFromEnv() []string {
	raw := strings.TrimSpace(os.Getenv("FTRW_BROWSER_ARGS"))
	if raw == "" {
		return nil
	}
	args := strings.Fields(raw)
	log.Printf("调试浏览器参数：%v", args)
	return args
}

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
	// 全局代理先落到内存里那一份：两个 http.Transport 的 DialContext 只看它，不看 store。
	setGlobalProxy(proxyFromPrefs(store.Prefs))

	configService := NewConfigService(store)
	terminalService := NewTerminalService(store)
	appService := NewAppService(store)
	previewService := NewPreviewService()
	launcherService := NewLauncherService()
	ipService := NewIPService(store)
	fileService := NewFileService()
	// 代理没配好之前 IP 库注定下不来，失败一轮就要等满一天闸门。用户刚刚把代理填对的
	// 那一刻就是「该再试一次」的信号，不能让他等到明天。
	configService.OnProxyChanged(func() { ipService.MaybeUpdate() })
	// 偏好保存后把搜索范围那份交给启动器并重建索引：用户改完目录/后缀点保存，
	// 下次呼出就该看到新东西，不该还要重启。启动路径上不重建：Init 紧跟着就建。
	launcherService.ApplyPrefs(store.Prefs)
	configService.OnPrefsChanged(func(p Preferences) {
		launcherService.ApplyPrefs(p)
		go launcherService.LauncherRebuild()
	})

	exe, _ := os.Executable()
	log.Printf("FTRW 启动 pid=%d exe=%s log=%s", os.Getpid(), exe, logger.Path())

	app := application.New(application.Options{
		Name:        "FTRW",
		Description: "Telnet/SSH BBS Terminal (Wails 3 rewrite)",
		Logger:      newSystemLogger(out),
		// 调试通路：FTRW_BROWSER_ARGS="--remote-debugging-port=9444" 时把参数直接交给
		// WebView2。鼠标坐标、单元格像素、缓冲区文本这些只在运行时才有的事实，靠读代码
		// 已经连着猜了两轮，留一条能用 CDP 亲眼看的路子比再猜一轮便宜。
		// 注：这项一旦显式设上，WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS 环境变量会被顶掉，所以走这里。
		Windows: application.WindowsOptions{
			AdditionalBrowserArgs: browserArgsFromEnv(),
		},
		Services: []application.Service{
			application.NewService(configService),
			application.NewService(terminalService),
			application.NewService(appService),
			application.NewService(previewService),
			application.NewService(launcherService),
			application.NewService(ipService),
			application.NewService(fileService),
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
	appService.SetLauncher(launcherService)
	previewService.Bind(app, win, appService)
	launcherService.Bind(app, win, appService)
	ipService.Bind(win)

	// 跨应用热键不在这里注册：低级键盘钩子要等应用跑起来之后才能安全地往主线程投递动作，
	// 见 AppService.Start 里的 startGlobalKeys。
	// 托盘必须在 Run 之前建：New() 在应用已运行时会立即把图标加进通知区域，
	// 赶在 SetIcon 之前就只能拿到 Wails 的回退图标。
	appService.SetupTray()
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		appService.Start()
		// 启动器预建隐藏窗 + 后台建索引：热键呼出时只剩 Show 与抢前台，不为 webview
		// 冷启动买单。
		launcherService.Init()
		// 库过期或缺过一次就后台补一轮：开在启动路径上是白等，而且失败也不该拦人。
		go ipService.MaybeUpdate()
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
