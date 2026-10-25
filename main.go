package main

import (
	"context"
	"embed"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend
var frontendFiles embed.FS

func main() {
	assets, err := fs.Sub(frontendFiles, "frontend")
	if err != nil {
		log.Fatal(err)
	}

	app := NewApp()

	winOpts := &windows.Options{
		Theme:                windows.SystemDefault,
		IsZoomControlEnabled: true,
		ZoomFactor:           1.0,
	}
	if app.paths.WebviewDir != "" {
		winOpts.WebviewUserDataPath = app.paths.WebviewDir
	}

	err = wails.Run(&options.App{
		Title:     "Homebox",
		Width:     1280,
		Height:    840,
		MinWidth:  420,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour:         options.NewRGB(246, 245, 242),
		Menu:                     app.buildMenu(),
		OnStartup:                app.startup,
		OnShutdown:               app.shutdown,
		EnableDefaultContextMenu: true,
		Bind:                     []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "homebox-desktop.single-instance.7b1f3c2e",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				if app.ctx != nil {
					wruntime.WindowUnminimise(app.ctx)
					wruntime.WindowShow(app.ctx)
				}
			},
		},
		Windows: winOpts,
		OnBeforeClose: func(ctx context.Context) bool {
			// Don't let the window close in the middle of a backup or restore.
			if app.opMu.TryLock() {
				app.opMu.Unlock()
				return false
			}
			go app.info("Please wait", "Homebox Desktop is finishing a task (such as a backup or restore). Close it again once that has finished.")
			return true
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
