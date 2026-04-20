package main

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

type forcedVariantTheme struct {
	dark bool
}

func (f *forcedVariantTheme) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	if f.dark {
		return theme.DefaultTheme().Color(n, theme.VariantDark)
	}
	return theme.DefaultTheme().Color(n, theme.VariantLight)
}
func (f *forcedVariantTheme) Font(s fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(s)
}
func (f *forcedVariantTheme) Icon(n fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(n)
}
func (f *forcedVariantTheme) Size(n fyne.ThemeSizeName) float32 { return theme.DefaultTheme().Size(n) }

func main() {
	myApp := app.New()
	mainWindow := myApp.NewWindow("P2P Sync Engine")
	mainWindow.Resize(fyne.NewSize(600, 450))

	appTheme := &forcedVariantTheme{dark: true}
	myApp.Settings().SetTheme(appTheme)

	idEntry := widget.NewEntry()
	idEntry.SetText("Machine-A")

	folderEntry := widget.NewEntry()
	folderEntry.SetPlaceHolder("Example: ./SyncFolder")

	serverEntry := widget.NewEntry()
	serverEntry.SetText("127.0.0.1:9000")

	themeSelect := widget.NewSelect([]string{"Dark", "Light"}, func(selected string) {
		appTheme.dark = (selected == "Dark")
		myApp.Settings().SetTheme(appTheme)
	})
	themeSelect.SetSelected("Dark")

	logArea := widget.NewMultiLineEntry()
	logArea.Disable()
	logArea.SetText("System Ready. Awaiting connection...\n")

	UILogCallback = func(msg string) {
		currentText := logArea.Text
		logArea.SetText(currentText + msg + "\n")
		logArea.CursorColumn = 0
		logArea.CursorRow = len(logArea.Text)
	}

	var connectBtn *widget.Button
	var disconnectBtn *widget.Button

	toggleInputs := func(enabled bool) {
		if enabled {
			idEntry.Enable()
			folderEntry.Enable()
			serverEntry.Enable()
			themeSelect.Enable()
		} else {
			idEntry.Disable()
			folderEntry.Disable()
			serverEntry.Disable()
			themeSelect.Disable()
		}
	}

	connectBtn = widget.NewButton("Connect & Start Syncing", func() {
		if idEntry.Text == "" || folderEntry.Text == "" || serverEntry.Text == "" {
			UILogCallback("[ERROR] Please fill in all fields before connecting.\n")
			return
		}

		connectBtn.Disable()
		toggleInputs(false)

		err := StartEngine(idEntry.Text, folderEntry.Text, serverEntry.Text)

		if err != nil {
			connectBtn.Enable()
			toggleInputs(true)
		} else {
			disconnectBtn.Enable()
		}
	})

	disconnectBtn = widget.NewButton("Stop Syncing", func() {
		StopEngine()
		disconnectBtn.Disable()
		connectBtn.Enable()
		toggleInputs(true)
	})
	disconnectBtn.Disable()

	UIDisconnectCallback = func() {
		disconnectBtn.Disable()
		connectBtn.Enable()
		toggleInputs(true)
	}

	form := widget.NewForm(
		widget.NewFormItem("Client ID", idEntry),
		widget.NewFormItem("Folder Path", folderEntry),
		widget.NewFormItem("Relay Server", serverEntry),
		widget.NewFormItem("Theme", themeSelect),
	)

	buttonBox := container.NewGridWithColumns(2, connectBtn, disconnectBtn)
	topSection := container.NewVBox(form, buttonBox)

	mainContent := container.NewBorder(topSection, nil, nil, nil, logArea)
	mainWindow.SetContent(mainContent)
	mainWindow.ShowAndRun()
}
