package main

import "github.com/wailsapp/wails/v3/pkg/application"

// panelOptions — панель тостов на macOS: NSPanel, который не делает приложение активным, на всех
// рабочих столах и поверх полноэкранных окон, без участия в Cmd-`.
func panelOptions(o *application.WebviewWindowOptions) {
	o.Mac = application.MacWindow{
		WindowClass:      application.MacWindowClassPanel,
		PanelPreferences: application.MacPanelPreferences{NonActivating: true},
		WindowLevel:      application.MacWindowLevelFloating,
		CollectionBehavior: application.MacWindowCollectionBehaviorCanJoinAllSpaces |
			application.MacWindowCollectionBehaviorFullScreenAuxiliary |
			application.MacWindowCollectionBehaviorStationary |
			application.MacWindowCollectionBehaviorIgnoresCycle,
	}
}

func panelWarmUp(*application.WebviewWindow) {}

// panelShow: такую панель Wails выводит через orderFrontRegardless, не делая её ключевым окном.
func panelShow(w *application.WebviewWindow) { w.Show() }

func panelHide(w *application.WebviewWindow) { w.Hide() }
