package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const maxSleepMinutes = 24 * 60

var sleepMenuMinutes = []int{15, 30, 60}

func parseSleepMinutes(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, errors.New("sleep requires minutes or off")
	}
	if strings.EqualFold(s, "off") {
		return 0, nil
	}
	minutes, err := strconv.Atoi(s)
	if err != nil || minutes < 0 || minutes > maxSleepMinutes {
		return 0, fmt.Errorf("invalid sleep duration %q (0-%d minutes or off)", raw, maxSleepMinutes)
	}
	return minutes, nil
}

func (a *trayApp) setSleepTimer(minutes int) error {
	if minutes < 0 || minutes > maxSleepMinutes {
		return fmt.Errorf("sleep duration must be 0-%d minutes", maxSleepMinutes)
	}
	if minutes == 0 {
		a.cancelSleepTimer()
		return nil
	}
	a.armSleep(time.Duration(minutes) * time.Minute)
	slog.Info("sleep timer set", "minutes", minutes)
	return nil
}

func (a *trayApp) armSleep(d time.Duration) {
	a.sleepMu.Lock()
	if a.sleepTimer != nil {
		a.sleepTimer.Stop()
	}
	a.sleepGen++
	gen := a.sleepGen
	a.sleepDuration = d
	a.sleepTimer = time.AfterFunc(d, func() { a.sleepExpired(gen) })
	a.sleepMu.Unlock()
	a.updateSleepMenu()
}

func (a *trayApp) cancelSleepTimer() {
	a.sleepMu.Lock()
	if a.sleepTimer != nil {
		a.sleepTimer.Stop()
	}
	a.sleepTimer = nil
	a.sleepDuration = 0
	a.sleepGen++
	a.sleepMu.Unlock()
	a.updateSleepMenu()
}

func (a *trayApp) sleepTimerMinutes() int {
	a.sleepMu.Lock()
	defer a.sleepMu.Unlock()
	return int(a.sleepDuration / time.Minute)
}

func (a *trayApp) sleepExpired(gen uint64) {
	a.sleepMu.Lock()
	if gen != a.sleepGen || a.sleepTimer == nil {
		a.sleepMu.Unlock()
		return
	}
	a.sleepTimer = nil
	a.sleepDuration = 0
	a.sleepMu.Unlock()

	active := a.player.Snapshot().Status != StatusStopped
	if active {
		if err := a.player.Stop(); err != nil {
			slog.Error("sleep timer stop", "err", err)
		}
		notifyMessage("Priyomnik", "Sleep timer stopped playback.")
	}
	slog.Info("sleep timer expired", "was_active", active)
	a.updateSleepMenu()
}

func (a *trayApp) updateSleepMenu() {
	minutes := a.sleepTimerMinutes()

	a.menuMu.Lock()
	defer a.menuMu.Unlock()
	if a.sleepItem == nil {
		return
	}
	if minutes > 0 {
		a.sleepItem.SetTitle(fmt.Sprintf("Sleep timer: %dm", minutes))
	} else {
		a.sleepItem.SetTitle("Sleep timer")
	}
	for preset, item := range a.sleepPresets {
		if preset == minutes {
			item.Check()
		} else {
			item.Uncheck()
		}
	}
	if a.sleepOffItem != nil {
		if minutes == 0 {
			a.sleepOffItem.Check()
		} else {
			a.sleepOffItem.Uncheck()
		}
	}
}
