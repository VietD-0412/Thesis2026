package main

import (
	"fmt"
	"image/color"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

type appTheme struct{ dark bool }

func (t *appTheme) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	v := theme.VariantDark
	if !t.dark {
		v = theme.VariantLight
	}
	return theme.DefaultTheme().Color(n, v)
}
func (t *appTheme) Font(s fyne.TextStyle) fyne.Resource     { return theme.DefaultTheme().Font(s) }
func (t *appTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return theme.DefaultTheme().Icon(n) }
func (t *appTheme) Size(n fyne.ThemeSizeName) float32       { return theme.DefaultTheme().Size(n) }

var (
	colBackground  = color.NRGBA{R: 14, G: 18, B: 24, A: 255}
	colSurface     = color.NRGBA{R: 20, G: 26, B: 35, A: 255}
	colSurfaceHigh = color.NRGBA{R: 28, G: 36, B: 48, A: 255}
	colBorder      = color.NRGBA{R: 45, G: 58, B: 75, A: 255}

	colAccent     = color.NRGBA{R: 56, G: 139, B: 253, A: 255}
	colAccentDim  = color.NRGBA{R: 56, G: 139, B: 253, A: 60}
	colAccentSoft = color.NRGBA{R: 130, G: 180, B: 255, A: 255}
	colSuccess    = color.NRGBA{R: 35, G: 197, B: 94, A: 255}
	colDanger     = color.NRGBA{R: 218, G: 54, B: 51, A: 255}
	colWarning    = color.NRGBA{R: 210, G: 153, B: 34, A: 255}

	colTextPrimary = color.NRGBA{R: 220, G: 230, B: 242, A: 255}
	colTextMuted   = color.NRGBA{R: 110, G: 128, B: 150, A: 255}
	colTextDim     = color.NRGBA{R: 65, G: 80, B: 100, A: 255}

	colOnline  = colSuccess
	colOffline = colDanger

	colSentText     = colAccentSoft
	colReceivedText = color.NRGBA{R: 92, G: 210, B: 255, A: 255}
)

type statusDot struct {
	widget.BaseWidget
	online bool
}

func newStatusDot(online bool) *statusDot {
	d := &statusDot{online: online}
	d.ExtendBaseWidget(d)
	return d
}
func (d *statusDot) SetOnline(v bool) { d.online = v; d.Refresh() }
func (d *statusDot) CreateRenderer() fyne.WidgetRenderer {
	circle := canvas.NewCircle(colOffline)
	circle.Resize(fyne.NewSize(10, 10))
	return &statusDotRenderer{dot: d, circle: circle}
}

type statusDotRenderer struct {
	dot    *statusDot
	circle *canvas.Circle
}

func (r *statusDotRenderer) Layout(s fyne.Size) { r.circle.Resize(s) }
func (r *statusDotRenderer) MinSize() fyne.Size { return fyne.NewSize(10, 10) }
func (r *statusDotRenderer) Refresh() {
	if r.dot.online {
		r.circle.FillColor = colOnline
	} else {
		r.circle.FillColor = colOffline
	}
	r.circle.Refresh()
}
func (r *statusDotRenderer) Destroy()                     {}
func (r *statusDotRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.circle} }

type eventKind int

const (
	evSent eventKind = iota
	evReceived
	evDeleted
	evConflict
)

type syncEvent struct {
	kind      eventKind
	fileName  string
	timestamp time.Time
}

type peer struct {
	id     string
	online bool
}

func main() {
	myApp := app.NewWithID("com.relay.syncclient")
	win := myApp.NewWindow("Relay Sync")
	win.Resize(fyne.NewSize(1100, 680))

	currentTheme := &appTheme{dark: true}
	myApp.Settings().SetTheme(currentTheme)

	var (
		connected bool
		eventsMu  sync.Mutex
		events    []syncEvent
		peersMu   sync.Mutex
		peers     []peer
	)

	dot := newStatusDot(false)
	statusLabel := canvas.NewText("Disconnected", colOffline)
	statusLabel.TextStyle = fyne.TextStyle{Bold: true}
	statusLabel.TextSize = 13

	serverLabel := canvas.NewText("", colTextMuted)
	serverLabel.TextSize = 12

	idEntry := widget.NewEntry()
	idEntry.SetPlaceHolder("e.g. Machine-A")

	// swarmKeyEntry := widget.NewPasswordEntry()
	// swarmKeyEntry.SetPlaceHolder("Shared group password")

	folderEntry := widget.NewEntry()
	folderEntry.SetPlaceHolder("Path to sync folder")

	serverEntry := widget.NewEntry()
	serverEntry.SetText("127.0.0.1:9000")

	browseBtn := widget.NewButtonWithIcon("Browse", theme.FolderOpenIcon(), nil)
	browseBtn.OnTapped = func() {
		dialog.ShowFolderOpen(func(uri fyne.ListableURI, err error) {
			if err != nil || uri == nil {
				return
			}
			folderEntry.SetText(uri.Path())
		}, win)
	}
	folderRow := container.NewBorder(nil, nil, nil, browseBtn, folderEntry)

	themeCheck := widget.NewCheck("Light mode", func(light bool) {
		currentTheme.dark = !light
		myApp.Settings().SetTheme(currentTheme)
	})

	progressBar := widget.NewProgressBar()
	progressBar.Hide()
	progressLabel := canvas.NewText("", colTextMuted)
	progressLabel.TextSize = 11

	peerList := widget.NewList(
		func() int {
			peersMu.Lock()
			defer peersMu.Unlock()
			return len(peers)
		},
		func() fyne.CanvasObject {
			d := newStatusDot(false)
			lbl := canvas.NewText("", colTextPrimary)
			lbl.TextSize = 13
			return container.NewHBox(d, lbl)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			peersMu.Lock()
			if i >= len(peers) {
				peersMu.Unlock()
				return
			}
			p := peers[i]
			peersMu.Unlock()

			row := o.(*fyne.Container)
			row.Objects[0].(*statusDot).SetOnline(p.online)
			lbl := row.Objects[1].(*canvas.Text)
			lbl.Text = p.id
			if p.online {
				lbl.Color = colTextPrimary
			} else {
				lbl.Color = colTextMuted
			}
			lbl.Refresh()
		},
	)

	addOrUpdatePeer := func(id string, online bool) {
		peersMu.Lock()
		found := false
		for i, p := range peers {
			if p.id == id {
				peers[i].online = online
				found = true
				break
			}
		}
		if !found {
			peers = append(peers, peer{id: id, online: online})
		}
		peersMu.Unlock()
		peerList.Refresh()
	}

	// Event log table
	addEvent := func(ev syncEvent) {
		eventsMu.Lock()
		events = append([]syncEvent{ev}, events...)
		if len(events) > 200 {
			events = events[:200]
		}
		eventsMu.Unlock()
	}

	evTable := widget.NewTable(
		func() (int, int) {
			eventsMu.Lock()
			defer eventsMu.Unlock()
			return len(events), 3
		},
		func() fyne.CanvasObject {
			return canvas.NewText("", colTextPrimary)
		},
		func(id widget.TableCellID, o fyne.CanvasObject) {
			eventsMu.Lock()
			if id.Row >= len(events) {
				eventsMu.Unlock()
				return
			}
			ev := events[id.Row]
			eventsMu.Unlock()

			t := o.(*canvas.Text)
			t.TextSize = 12

			switch id.Col {
			case 0:
				t.Text = ev.timestamp.Format("15:04:05")
				t.Color = colTextDim
			case 1:
				switch ev.kind {
				case evSent:
					t.Text = "↑ SENT"
					t.Color = colSentText
				case evReceived:
					t.Text = "↓ RECEIVED"
					t.Color = colReceivedText
				case evDeleted:
					t.Text = "✕ DELETED"
					t.Color = colDanger
				case evConflict:
					t.Text = "⚠ CONFLICT"
					t.Color = colWarning
				}
			case 2: // filename
				t.Text = ev.fileName
				t.Color = colTextPrimary
			}
			t.Refresh()
		},
	)
	evTable.SetColumnWidth(0, 72)
	evTable.SetColumnWidth(1, 100)
	evTable.SetColumnWidth(2, 400)

	logEntry := widget.NewMultiLineEntry()
	logEntry.Disable()
	logEntry.SetText("System ready.\n")
	logEntry.TextStyle = fyne.TextStyle{Monospace: true}

	logScroll := container.NewScroll(logEntry)
	logScroll.SetMinSize(fyne.NewSize(0, 180))
	logCard := widget.NewAccordion(
		widget.NewAccordionItem("Diagnostic Log", logScroll),
	)
	logCard.Open(0)

	appendLog := func(msg string) {
		logEntry.SetText(logEntry.Text + msg)
	}

	var connectBtn, disconnectBtn *widget.Button

	setConnected := func(c bool) {
		connected = c
		dot.SetOnline(c)
		if c {
			statusLabel.Text = "Connected"
			statusLabel.Color = colOnline
			serverLabel.Text = "  ●  " + serverEntry.Text
		} else {
			statusLabel.Text = "Disconnected"
			statusLabel.Color = colOffline
			serverLabel.Text = ""
			peersMu.Lock()
			for i := range peers {
				peers[i].online = false
			}
			peersMu.Unlock()
			peerList.Refresh()
		}
		statusLabel.Refresh()
		serverLabel.Refresh()

		if c {
			connectBtn.Disable()
			disconnectBtn.Enable()
			idEntry.Disable()
			// swarmKeyEntry.Disable()
			folderEntry.Disable()
			serverEntry.Disable()
			browseBtn.Disable()
		} else {
			connectBtn.Enable()
			disconnectBtn.Disable()
			idEntry.Enable()
			// swarmKeyEntry.Enable()
			folderEntry.Enable()
			serverEntry.Enable()
			browseBtn.Enable()
			progressBar.Hide()
		}
		_ = connected
	}

	connectBtn = widget.NewButton("Connect", func() {
		if strings.TrimSpace(idEntry.Text) == "" ||
			strings.TrimSpace(folderEntry.Text) == "" ||
			strings.TrimSpace(serverEntry.Text) == "" {
			dialog.ShowInformation("Missing fields", "Please fill in Client ID, Folder Path, and Relay Server.", win)
			return
		}
		connectBtn.Disable()
		err := StartEngine(idEntry.Text, folderEntry.Text, serverEntry.Text, "")
		if err != nil {
			connectBtn.Enable()
			dialog.ShowError(err, win)
			return
		}
		setConnected(true)
	})

	disconnectBtn = widget.NewButton("Disconnect", func() {
		StopEngine()
		setConnected(false)
	})
	disconnectBtn.Disable()

	UILogCallback = func(msg string) {
		trimmed := strings.TrimSpace(msg)
		fyne.Do(func() {
			appendLog(msg)

			switch {
			case strings.Contains(trimmed, "File pushed"):
				addEvent(syncEvent{kind: evSent, fileName: extractFileName(trimmed), timestamp: time.Now()})
				evTable.Refresh()

			case strings.Contains(trimmed, "File successfully synced"):
				addEvent(syncEvent{kind: evReceived, fileName: extractFileName(trimmed), timestamp: time.Now()})
				evTable.Refresh()

			case strings.Contains(trimmed, "[WATCHER] Local deletion"):
				fn := strings.TrimPrefix(trimmed, "[WATCHER] Local deletion detected: ")
				addEvent(syncEvent{kind: evDeleted, fileName: fn, timestamp: time.Now()})
				evTable.Refresh()

			case strings.Contains(trimmed, "[CONFLICT]"):
				parts := strings.SplitN(trimmed, " ", 3)
				fn := ""
				if len(parts) >= 2 {
					fn = parts[1]
				}
				addEvent(syncEvent{kind: evConflict, fileName: fn, timestamp: time.Now()})
				evTable.Refresh()
				dialog.ShowInformation("Sync Conflict",
					fmt.Sprintf("Conflict on file:\n%s\n\nPrevious version saved as .conflict", fn), win)
			}
		})
	}

	UIPeerCallback = func(peerID string, online bool) {
		fyne.Do(func() {
			addOrUpdatePeer(peerID, online)
		})
	}

	UIProgressCallback = func(val float64) {
		fyne.Do(func() {
			if val <= 0 {
				progressBar.Hide()
				progressLabel.Text = ""
				progressLabel.Refresh()
				return
			}
			progressBar.Show()
			progressBar.SetValue(val)
			progressLabel.Text = fmt.Sprintf("%.0f%%", val*100)
			progressLabel.Refresh()
		})

	}

	UIDisconnectCallback = func() {
		fyne.Do(func() {
			setConnected(false)
			appendLog("[SYSTEM] Lost connection to relay server.\n")
		})
	}

	// LAYOUT
	statusBar := container.NewHBox(
		container.NewPadded(dot),
		statusLabel,
		serverLabel,
		layout.NewSpacer(),
		themeCheck,
	)
	statusBarBg := canvas.NewRectangle(colSurface)
	statusBarFull := container.NewStack(statusBarBg, container.NewPadded(statusBar))

	//Left sidebar
	configForm := container.NewVBox(
		sectionLabel("CONNECTION"),
		labeledField("Client ID", idEntry),
		// labeledField("Group Key", swarmKeyEntry),
		labeledField("Sync Folder", folderRow),
		labeledField("Relay Server", serverEntry),
		widget.NewSeparator(),
		container.NewGridWithColumns(2, connectBtn, disconnectBtn),
		widget.NewSeparator(),
		sectionLabel("TRANSFER"),
		progressBar,
		progressLabel,
	)
	configScroll := container.NewVScroll(configForm)
	configScroll.SetMinSize(fyne.NewSize(260, 0))

	configCard := container.NewStack(
		canvas.NewRectangle(colSurface),
		container.NewPadded(configScroll),
	)

	peersHeader := sectionLabel("PEERS")
	peersCard := container.NewStack(
		canvas.NewRectangle(colSurface),
		container.NewBorder(
			container.NewPadded(peersHeader), nil, nil, nil,
			peerList,
		),
	)

	eventsHeader := sectionLabel("SYNC ACTIVITY")
	eventsCard := container.NewStack(
		canvas.NewRectangle(colSurface),
		container.NewBorder(
			container.NewPadded(eventsHeader), nil, nil, nil,
			evTable,
		),
	)

	// Right sidebar
	rightTop := eventsCard
	rightBottom := container.NewHSplit(peersCard, canvas.NewRectangle(colBackground))
	rightBottom.Offset = 0.4
	bottomSection := container.NewBorder(nil, logCard, nil, nil, rightBottom)
	rightSplit := container.NewVSplit(rightTop, bottomSection)
	rightSplit.Offset = 0.65

	mainSplit := container.NewHSplit(configCard, rightSplit)
	mainSplit.Offset = 0.26

	root := container.NewBorder(statusBarFull, nil, nil, nil, mainSplit)
	win.SetContent(root)
	win.ShowAndRun()
}

func sectionLabel(text string) *canvas.Text {
	t := canvas.NewText(text, colAccentSoft)
	t.TextSize = 10
	t.TextStyle = fyne.TextStyle{Bold: true}
	return t
}

func labeledField(label string, w fyne.CanvasObject) *fyne.Container {
	lbl := canvas.NewText(label, colTextMuted)
	lbl.TextSize = 11
	return container.NewVBox(lbl, w)
}

func extractFileName(msg string) string {
	parts := strings.Fields(msg)
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.Contains(parts[i], ".") {
			return parts[i]
		}
	}
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "unknown"
}
