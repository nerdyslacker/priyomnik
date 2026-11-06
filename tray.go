package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"fyne.io/systray"
)

//go:embed assets/icons/stopped.png
var iconStopped []byte

//go:embed assets/icons/connecting.png
var iconConnecting []byte

//go:embed assets/icons/playing.png
var iconPlaying []byte

//go:embed assets/icons/error.png
var iconError []byte

type trayApp struct {
	cfgPath     string
	mu          sync.RWMutex
	cfg         *Config
	selected    string
	player      *Player
	notifier    *notifier
	controlSock string
	control     *controlServer
	ctx         context.Context
	cancel      context.CancelFunc
	dialogMu    sync.Mutex
	mpris       atomic.Pointer[mprisServer]

	menuMu       sync.Mutex
	built        bool
	lastState    State
	lastStatus   Status
	lastSelected string
	lastStop     bool
	lastVolume   int
	lastMute     bool
	stationItems map[string]*systray.MenuItem
	infoItem     *systray.MenuItem
	stopItem     *systray.MenuItem
	volumeItem   *systray.MenuItem
	muteItem     *systray.MenuItem
}

func runTray() int {
	sock, err := controlSocketPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "priyomnik: %v\n", err)
		return 1
	}
	lock, err := acquireControlLock(sock)
	if errors.Is(err, errAlreadyRunning) {
		fmt.Fprintln(os.Stderr, "priyomnik: already running")
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "priyomnik: %v\n", err)
		return 1
	}
	defer lock.Close()

	path, err := ConfigPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "priyomnik: %v\n", err)
		return 1
	}
	cfg, err := Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "priyomnik: %v\n", err)
		return 1
	}

	player := NewPlayer(cfg.Volume)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := &trayApp{
		cfgPath:     path,
		cfg:         cfg,
		selected:    cfg.LastStation,
		player:      player,
		notifier:    newNotifier(),
		controlSock: sock,
		ctx:         ctx,
		cancel:      cancel,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		systray.Quit()
	}()

	systray.Run(app.onReady, func() {
		app.cancel()
		app.closeControl()
		app.notifier.Close()
		if err := player.Close(); err != nil {
			slog.Error("close player", "err", err)
		}
	})
	return 0
}

func (a *trayApp) onReady() {
	a.buildMenu()
	systray.SetOnSecondaryTapped(func() {
		go a.toggle()
	})
	systray.SetOnScroll(func(delta int, orientation string) {
		if delta == 0 || (orientation != "" && orientation != "vertical") {
			return
		}
		dir := 1
		if delta < 0 {
			dir = -1
		}
		go a.adjustVolume(dir)
	})
	if err := a.startControl(); err != nil {
		slog.Error("start control socket", "err", err)
	}
	a.mpris.Store(startMPRIS(a))
	go a.watchPlayer()

	cfg := a.copyConfig()
	if cfg.Autoplay {
		if st, ok := findStation(cfg.Stations, cfg.LastStation); ok {
			go func() {
				if err := a.playStation(st); err != nil {
					slog.Error("autoplay", "station", st.ID, "err", err)
				}
			}()
		}
	}
}

func (a *trayApp) watchPlayer() {
	for range a.player.Updates() {
		a.refresh()
	}
}

func (a *trayApp) buildMenu() {
	a.menuMu.Lock()

	systray.ResetMenu()
	cfg := a.copyConfig()
	a.stationItems = make(map[string]*systray.MenuItem, len(cfg.Stations))

	header := systray.AddMenuItem("Priyomnik", "Priyomnik")
	header.Disable()

	groups := make(map[string]*systray.MenuItem)
	for _, st := range cfg.Stations {
		var item *systray.MenuItem
		if st.Group == "" {
			item = systray.AddMenuItemCheckbox(st.Name, st.URL, false)
		} else {
			group, ok := groups[st.Group]
			if !ok {
				group = systray.AddMenuItem(st.Group, st.Group)
				groups[st.Group] = group
			}
			item = group.AddSubMenuItemCheckbox(st.Name, st.URL, false)
		}
		a.stationItems[st.ID] = item

		id := st.ID
		go func(item *systray.MenuItem, id string) {
			for range item.ClickedCh {
				go a.playByID(id)
			}
		}(item, id)
	}

	systray.AddSeparator()
	a.infoItem = systray.AddMenuItem("Stopped", "")
	a.infoItem.Disable()

	stop := systray.AddMenuItem("Stop", "Stop playback")
	a.stopItem = stop
	go func() {
		for range stop.ClickedCh {
			go a.stop()
		}
	}()

	volume := systray.AddMenuItem("Volume: 0%", "Volume")
	a.volumeItem = volume
	up := volume.AddSubMenuItem("Volume up", "")
	down := volume.AddSubMenuItem("Volume down", "")
	mute := volume.AddSubMenuItemCheckbox("Mute", "", false)
	a.muteItem = mute
	go func() {
		for range up.ClickedCh {
			go a.adjustVolume(1)
		}
	}()
	go func() {
		for range down.ClickedCh {
			go a.adjustVolume(-1)
		}
	}()
	go func() {
		for range mute.ClickedCh {
			go a.toggleMute()
		}
	}()

	systray.AddSeparator()
	edit := systray.AddMenuItem("Edit stations...", "Add, edit, or remove stations")
	go func() {
		for range edit.ClickedCh {
			go a.editStations()
		}
	}()
	reload := systray.AddMenuItem("Reload stations", "Reload configuration")
	go func() {
		for range reload.ClickedCh {
			go func() {
				if err := a.reload(); err != nil {
					slog.Error("reload config", "err", err)
				}
			}()
		}
	}()
	quit := systray.AddMenuItem("Quit", "Quit Priyomnik")
	go func() {
		for range quit.ClickedCh {
			go a.quit()
		}
	}()

	a.built = true
	a.lastState = State{}
	a.lastStatus = ""
	a.lastSelected = ""
	a.lastStop = true
	a.lastVolume = -1
	a.lastMute = false
	a.menuMu.Unlock()
	a.refresh()
}

func (a *trayApp) refresh() {
	st := a.player.Snapshot()
	if st.StationID != "" {
		cfg := a.copyConfig()
		if station, ok := findStation(cfg.Stations, st.StationID); ok {
			st.StationName = station.Name
		}
	}

	a.menuMu.Lock()
	if !a.built {
		a.menuMu.Unlock()
		return
	}
	if a.lastState == st {
		a.menuMu.Unlock()
		return
	}

	selected := st.StationID
	if selected == "" {
		a.mu.RLock()
		selected = a.selected
		a.mu.RUnlock()
	}
	if selected != a.lastSelected {
		for id, item := range a.stationItems {
			if id == selected {
				item.Check()
			} else {
				item.Uncheck()
			}
		}
		a.lastSelected = selected
	}

	stopEnabled := st.Status != StatusStopped
	if stopEnabled != a.lastStop {
		if stopEnabled {
			a.stopItem.Enable()
		} else {
			a.stopItem.Disable()
		}
		a.lastStop = stopEnabled
	}

	if st.Volume != a.lastVolume {
		a.volumeItem.SetTitle(fmt.Sprintf("Volume: %d%%", st.Volume))
		a.lastVolume = st.Volume
	}
	if st.Mute != a.lastMute {
		if st.Mute {
			a.muteItem.Check()
		} else {
			a.muteItem.Uncheck()
		}
		a.lastMute = st.Mute
	}
	a.infoItem.SetTitle(infoText(st))

	if st.Status != a.lastStatus {
		systray.SetIcon(iconFor(st.Status))
		a.lastStatus = st.Status
	}
	systray.SetTooltip(cleanText(tooltipText(st)))

	if st.Status == StatusPlaying && a.notificationsEnabled() {
		a.notifier.enqueue(st)
	}
	a.lastState = st
	a.menuMu.Unlock()

	if m := a.mpris.Load(); m != nil {
		m.update(st)
	}
}

func (a *trayApp) notificationsEnabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Notifications
}

func (a *trayApp) playByID(id string) {
	if err := a.playID(id); err != nil {
		slog.Error("play station", "station", id, "err", err)
	}
}

func (a *trayApp) playID(id string) error {
	cfg := a.copyConfig()
	st, ok := findStation(cfg.Stations, id)
	if !ok {
		return fmt.Errorf("unknown station %q", id)
	}
	return a.playStation(st)
}

func (a *trayApp) playStation(st Station) error {
	a.setSelected(st.ID)
	return a.player.Play(st)
}

func (a *trayApp) setSelected(id string) {
	a.mu.Lock()
	a.selected = id
	changed := a.cfg.LastStation != id
	a.mu.Unlock()
	if !changed {
		return
	}
	a.updateConfig(func(c *Config) {
		c.LastStation = id
	})
}

func (a *trayApp) stop() {
	if err := a.player.Stop(); err != nil {
		slog.Error("stop", "err", err)
	}
}

func (a *trayApp) adjustVolume(direction int) {
	cfg := a.copyConfig()
	if _, err := a.applyVolume(a.player.Snapshot().Volume + direction*cfg.VolumeStep); err != nil {
		slog.Error("adjust volume", "err", err)
	}
}

func (a *trayApp) toggleMute() {
	if _, err := a.player.ToggleMute(); err != nil {
		slog.Error("toggle mute", "err", err)
	}
}

func (a *trayApp) reload() error {
	cfg, err := Load(a.cfgPath)
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.cfg = cfg
	a.selected = cfg.LastStation
	a.mu.Unlock()

	a.buildMenu()
	if _, err := a.player.SetVolume(cfg.Volume); err != nil {
		return err
	}
	return nil
}

func (a *trayApp) quit() {
	systray.Quit()
}

func (a *trayApp) copyConfig() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	cfg := *a.cfg
	cfg.Stations = append([]Station(nil), a.cfg.Stations...)
	return cfg
}

func (a *trayApp) updateConfig(fn func(*Config)) {
	a.mu.Lock()
	cfg := *a.cfg
	cfg.Stations = append([]Station(nil), a.cfg.Stations...)
	fn(&cfg)
	if err := Save(a.cfgPath, &cfg); err != nil {
		slog.Error("save config", "err", err)
	}
	a.cfg = &cfg
	a.mu.Unlock()
}

func findStation(stations []Station, id string) (Station, bool) {
	for _, st := range stations {
		if st.ID == id {
			return st, true
		}
	}
	return Station{}, false
}

func infoText(st State) string {
	switch st.Status {
	case StatusPlaying:
		if st.Artist != "" && st.Title != "" {
			return "Now playing: " + st.Artist + " — " + st.Title
		}
		if st.Title != "" {
			return "Now playing: " + st.Title
		}
		if st.StationName != "" {
			return "Playing: " + st.StationName
		}
		return "Playing"
	case StatusConnecting:
		if st.StationName != "" {
			return "Connecting: " + st.StationName
		}
		return "Connecting"
	case StatusError:
		if st.Error != "" {
			return "Error: " + st.Error
		}
		return "Error"
	default:
		if st.StationName != "" {
			return st.StationName + " — stopped"
		}
		return "Stopped"
	}
}

func tooltipText(st State) string {
	s := st.StationName
	if st.Artist != "" && st.Title != "" {
		s += " — " + st.Artist + " — " + st.Title
	} else if st.Title != "" {
		s += " — " + st.Title
	}
	status := string(st.Status)
	if st.Status == StatusError && st.Error != "" {
		status += ": " + st.Error
	}
	if s == "" {
		return status
	}
	return s + " — " + status
}

func iconFor(st Status) []byte {
	switch st {
	case StatusPlaying:
		return iconPlaying
	case StatusConnecting:
		return iconConnecting
	case StatusError:
		return iconError
	default:
		return iconStopped
	}
}
