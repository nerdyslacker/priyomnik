package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const yadSep = "\x1f"

const (
	stationActionClose = iota
	stationActionNone
	stationActionAdd
	stationActionEdit
	stationActionRemove
)

// editStations runs the optional yad-based station editor. It is deliberately
// kept out of the tray callback: yad is an external GUI process.
func (a *trayApp) editStations() {
	if !a.dialogMu.TryLock() {
		return
	}
	defer a.dialogMu.Unlock()

	if _, err := exec.LookPath("yad"); err != nil {
		slog.Error("station editor", "err", err)
		notifyMessage("Priyomnik", "Station editor requires yad in PATH.")
		return
	}

	for {
		cfg := a.copyConfig()
		action, st, err := a.showStationList(cfg.Stations)
		if err != nil {
			slog.Error("station list dialog", "err", err)
			return
		}
		switch action {
		case stationActionAdd:
			a.addStationFlow()
		case stationActionEdit:
			a.editStationFlow(st)
		case stationActionRemove:
			a.removeStationFlow(st)
		case stationActionNone:
			continue
		default:
			return
		}
	}
}

func (a *trayApp) showStationList(stations []Station) (int, Station, error) {
	args := []string{
		"--list",
		"--title=Priyomnik stations",
		"--text=Select a station, then choose an action.",
		"--column=Name",
		"--column=Group",
		"--column=ID",
		"--column=URL",
		"--separator=" + yadSep,
		"--width=760",
		"--height=420",
		"--button=Add:2",
		"--button=Edit:4",
		"--button=Remove:6",
		"--button=Close:8",
		"--",
	}
	for _, st := range stations {
		args = append(args, st.Name, st.Group, st.ID, st.URL)
	}

	out, code, err := a.runYad(args...)
	if err != nil {
		return stationActionClose, Station{}, err
	}

	switch code {
	case 2:
		return stationActionAdd, Station{}, nil
	case 4, 0:
		if st, ok := parseStationRow(out); ok {
			if full, found := findStation(stations, st.ID); found {
				st.Volume = full.Volume
			}
			return stationActionEdit, st, nil
		}
		a.showError("Edit station", "Select a station first.")
		return stationActionNone, Station{}, nil
	case 6:
		if st, ok := parseStationRow(out); ok {
			return stationActionRemove, st, nil
		}
		a.showError("Remove station", "Select a station first.")
		return stationActionNone, Station{}, nil
	default:
		return stationActionClose, Station{}, nil
	}
}

func (a *trayApp) addStationFlow() {
	st := Station{}
	for {
		filled, ok := a.showStationForm(st, false)
		if !ok {
			return
		}
		st = filled

		cfg := a.copyConfig()
		if st.ID == "" {
			st.ID = uniqueStationID(cfg.Stations, st.Name)
		}
		cfg.Stations = append(cfg.Stations, st)
		if cfg.LastStation == "" {
			cfg.LastStation = st.ID
		}
		if err := a.applyStationEdit(&cfg); err != nil {
			a.showError("Cannot add station", err.Error())
			continue
		}
		return
	}
}

func (a *trayApp) editStationFlow(orig Station) {
	st := orig
	for {
		filled, ok := a.showStationForm(st, true)
		if !ok {
			return
		}
		if filled.ID == "" {
			filled.ID = orig.ID
		}
		st = filled

		cfg := a.copyConfig()
		idx := stationIndex(cfg.Stations, orig.ID)
		if idx < 0 {
			a.showError("Cannot edit station", "The station no longer exists.")
			return
		}
		cfg.Stations[idx] = st
		if cfg.LastStation == orig.ID && st.ID != orig.ID {
			cfg.LastStation = st.ID
		}
		if err := a.applyStationEdit(&cfg); err != nil {
			a.showError("Cannot edit station", err.Error())
			continue
		}
		return
	}
}

func (a *trayApp) removeStationFlow(st Station) {
	cfg := a.copyConfig()
	idx := stationIndex(cfg.Stations, st.ID)
	if idx < 0 {
		return
	}
	if !a.confirmRemove(cfg.Stations[idx].Name) {
		return
	}

	cfg.Stations = append(cfg.Stations[:idx], cfg.Stations[idx+1:]...)
	if cfg.LastStation == st.ID {
		cfg.LastStation = ""
	}
	if err := a.applyStationEdit(&cfg); err != nil {
		a.showError("Cannot remove station", err.Error())
	}
}

// applyStationEdit persists a validated configuration, rebuilds the menu, and
// keeps active playback consistent with the edited station list.
func (a *trayApp) applyStationEdit(cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := Save(a.cfgPath, cfg); err != nil {
		return err
	}

	a.mu.Lock()
	old := a.cfg
	a.cfg = cfg
	if _, ok := findStation(cfg.Stations, a.selected); !ok {
		a.selected = cfg.LastStation
	}
	a.mu.Unlock()

	a.buildMenu()

	active := a.player.Snapshot().StationID
	if active == "" {
		return nil
	}
	updated, ok := findStation(cfg.Stations, active)
	if !ok {
		_ = a.player.Stop()
		return nil
	}
	previous, had := findStation(old.Stations, active)
	if !had || previous.URL != updated.URL {
		a.restoreStationVolume(updated)
		if err := a.player.Play(updated); err != nil {
			slog.Error("restart edited station", "station", updated.ID, "err", err)
		}
		return nil
	}
	if !equalVolume(previous.Volume, updated.Volume) {
		a.restoreStationVolume(updated)
	}
	return nil
}

func equalVolume(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (a *trayApp) showStationForm(st Station, editing bool) (Station, bool) {
	title := "Add station"
	idLabel := "ID (blank = automatic)"
	if editing {
		title = "Edit station"
		idLabel = "ID"
	}

	for {
		volumeText := ""
		if st.Volume != nil {
			volumeText = strconv.Itoa(*st.Volume)
		}
		args := []string{
			"--form",
			"--title=" + title,
			"--separator=" + yadSep,
			"--field=Name",
			"--field=Group",
			"--field=" + idLabel,
			"--field=URL",
			"--field=Volume (0-100, blank = global)",
			"--width=520",
			"--button=Cancel:1",
			"--button=Save:0",
			"--",
			st.Name, st.Group, st.ID, st.URL, volumeText,
		}

		out, code, err := a.runYad(args...)
		if err != nil {
			slog.Error("station form dialog", "err", err)
			return Station{}, false
		}
		if code != 0 {
			return Station{}, false
		}

		fields := splitYadFields(out)
		if len(fields) < 5 {
			slog.Error("station form dialog", "err", "unexpected output", "output", out)
			return Station{}, false
		}
		filled := Station{
			Name:  cleanField(fields[0]),
			Group: cleanField(fields[1]),
			ID:    cleanField(fields[2]),
			URL:   cleanField(fields[3]),
		}
		stationVolume, err := parseStationVolume(fields[4])
		if err != nil {
			a.showError("Cannot save station", err.Error())
			st = filled
			continue
		}
		filled.Volume = stationVolume
		return filled, true
	}
}

// parseStationVolume returns nil for an empty value, meaning no station override.
func parseStationVolume(raw string) (*int, error) {
	raw = cleanField(raw)
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 || v > 100 {
		return nil, errors.New("volume must be a whole number from 0 to 100, or blank for the global volume")
	}
	return &v, nil
}

func (a *trayApp) confirmRemove(name string) bool {
	_, code, err := a.runYad(
		"--title=Remove station",
		"--image=dialog-question",
		"--text=Remove \""+name+"\"?",
		"--no-markup",
		"--button=No:1",
		"--button=Yes:0",
	)
	if err != nil {
		slog.Error("remove station dialog", "err", err)
		return false
	}
	return code == 0
}

func (a *trayApp) showError(title, message string) {
	_, _, err := a.runYad(
		"--title="+title,
		"--image=dialog-error",
		"--text="+message,
		"--no-markup",
		"--button=Close:0",
	)
	if err != nil {
		slog.Error("error dialog", "err", err)
	}
}

func (a *trayApp) runYad(args ...string) (string, int, error) {
	if icon := a.yadIconPath(); icon != "" {
		args = append([]string{"--window-icon=" + icon}, args...)
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "yad", args...)
	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code > 0 {
			return stdout.String(), code, nil
		}
	}
	if stderr.Len() > 0 {
		return "", -1, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return "", -1, err
}

func (a *trayApp) yadIconPath() string {
	if a.controlSock == "" {
		return ""
	}
	path := filepath.Join(filepath.Dir(a.controlSock), "dialog-icon.png")
	if err := os.WriteFile(path, iconDialog, 0o600); err != nil {
		return ""
	}
	return path
}

func splitYadFields(out string) []string {
	line := strings.TrimRight(out, "\r\n")
	if line == "" {
		return nil
	}
	fields := strings.Split(line, yadSep)
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	for _, f := range fields {
		if f != "" {
			return fields
		}
	}
	return nil
}

func parseStationRow(out string) (Station, bool) {
	fields := splitYadFields(out)
	if len(fields) < 4 || fields[2] == "" {
		return Station{}, false
	}
	return Station{
		Name:  fields[0],
		Group: fields[1],
		ID:    fields[2],
		URL:   fields[3],
	}, true
}

func cleanField(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func stationIndex(stations []Station, id string) int {
	for i, st := range stations {
		if st.ID == id {
			return i
		}
	}
	return -1
}

func uniqueStationID(stations []Station, name string) string {
	base := stationSlug(name)
	id := base
	for n := 2; stationIndex(stations, id) >= 0; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

func stationSlug(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "station"
	}
	return slug
}
