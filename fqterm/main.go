package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	application.RegisterEvent[*TermData]("term:data")
	application.RegisterEvent[*TermStatus]("term:status")
	application.RegisterEvent[*ZmodemProgress]("zmodem:progress")
}

func main() {
	store, err := NewStore()
	if err != nil {
		log.Fatalf("init config store: %v", err)
	}

	configService := NewConfigService(store)
	terminalService := NewTerminalService(store)

	app := application.New(application.Options{
		Name:        "FTRW",
		Description: "Telnet/SSH BBS Terminal (Wails 3 rewrite)",
		Services: []application.Service{
			application.NewService(configService),
			application.NewService(terminalService),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "FTRW - BBS 终端",
		Width:            1120,
		Height:           720,
		Frameless:        true,
		BackgroundColour: application.NewRGB(24, 26, 30),
		URL:              "/",
		StartState:       application.WindowStateNormal,
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
