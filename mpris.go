package main

import (
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	mprisName        = "org.mpris.MediaPlayer2.priyomnik"
	mprisRootIface   = "org.mpris.MediaPlayer2"
	mprisPlayerIface = "org.mpris.MediaPlayer2.Player"
	mprisNoTrack     = dbus.ObjectPath("/org/mpris/MediaPlayer2/TrackList/NoTrack")
)

var mprisPath = dbus.ObjectPath("/org/mpris/MediaPlayer2")

type mprisServer struct {
	app   *trayApp
	conn  *dbus.Conn
	props *prop.Properties

	mu           sync.Mutex
	trackKey     string
	trackSeq     uint64
	lastStatus   string
	lastVolume   float64
	lastMetadata bool
}

// startMPRIS exposes the standard MPRIS interfaces so desktop media keys and
// clients such as playerctl can control the running player. MPRIS is optional:
// a missing session bus or a name conflict only disables it.
func startMPRIS(app *trayApp) *mprisServer {
	conn, err := dbus.SessionBus()
	if err != nil {
		slog.Debug("mpris: session bus", "err", err)
		return nil
	}

	reply, err := conn.RequestName(mprisName, dbus.NameFlagDoNotQueue)
	if err != nil {
		slog.Warn("mpris: request name", "err", err)
		return nil
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		slog.Warn("mpris: name already owned", "name", mprisName)
		return nil
	}

	m := &mprisServer{app: app, conn: conn, lastVolume: -1}
	fail := func(step string, err error) *mprisServer {
		slog.Error("mpris: "+step, "err", err)
		_, _ = conn.ReleaseName(mprisName)
		return nil
	}

	if err := conn.Export(m, mprisPath, mprisRootIface); err != nil {
		return fail("export root interface", err)
	}
	// ExportWithMap avoids go vet's io.Seeker signature check on a DBus method
	// that must be named Seek.
	if err := conn.ExportWithMap(m, map[string]string{"SeekPosition": "Seek"},
		mprisPath, mprisPlayerIface); err != nil {
		return fail("export player interface", err)
	}

	props, err := prop.Export(conn, mprisPath, m.propMap())
	if err != nil {
		return fail("export properties", err)
	}
	m.props = props

	node := introspect.Node{
		Name: string(mprisPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			mprisRootIntrospection(props),
			mprisPlayerIntrospection(props),
		},
	}
	if err := conn.Export(introspect.NewIntrospectable(&node), mprisPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		return fail("export introspection", err)
	}

	m.update(app.statusState())
	slog.Info("mpris: available", "name", mprisName)
	return m
}

func (m *mprisServer) propMap() prop.Map {
	return prop.Map{
		mprisRootIface: {
			"Identity":            {Value: "Priyomnik", Emit: prop.EmitConst},
			"DesktopEntry":        {Value: "priyomnik", Emit: prop.EmitConst},
			"CanQuit":             {Value: true, Emit: prop.EmitConst},
			"CanRaise":            {Value: false, Emit: prop.EmitConst},
			"HasTrackList":        {Value: false, Emit: prop.EmitConst},
			"SupportedUriSchemes": {Value: []string{"http", "https"}, Emit: prop.EmitConst},
			"SupportedMimeTypes":  {Value: []string{}, Emit: prop.EmitConst},
		},
		mprisPlayerIface: {
			"PlaybackStatus": {Value: "Stopped", Emit: prop.EmitTrue},
			"LoopStatus":     {Value: "None", Emit: prop.EmitFalse},
			"Rate":           {Value: 1.0, Emit: prop.EmitFalse},
			"Shuffle":        {Value: false, Emit: prop.EmitFalse},
			"Metadata":       {Value: map[string]dbus.Variant{}, Emit: prop.EmitTrue},
			"Volume":         {Value: 0.7, Writable: true, Emit: prop.EmitTrue, Callback: m.setVolume},
			"Position":       {Value: int64(0), Emit: prop.EmitFalse},
			"MinimumRate":    {Value: 1.0, Emit: prop.EmitConst},
			"MaximumRate":    {Value: 1.0, Emit: prop.EmitConst},
			"CanGoNext":      {Value: true, Emit: prop.EmitConst},
			"CanGoPrevious":  {Value: true, Emit: prop.EmitConst},
			"CanPlay":        {Value: true, Emit: prop.EmitConst},
			"CanPause":       {Value: true, Emit: prop.EmitConst},
			"CanSeek":        {Value: false, Emit: prop.EmitConst},
			"CanControl":     {Value: true, Emit: prop.EmitConst},
		},
	}
}

func mprisRootIntrospection(props *prop.Properties) introspect.Interface {
	return introspect.Interface{
		Name: mprisRootIface,
		Methods: []introspect.Method{
			{Name: "Raise"},
			{Name: "Quit"},
		},
		Properties: props.Introspection(mprisRootIface),
	}
}

func mprisPlayerIntrospection(props *prop.Properties) introspect.Interface {
	return introspect.Interface{
		Name: mprisPlayerIface,
		Methods: []introspect.Method{
			{Name: "Next"},
			{Name: "Previous"},
			{Name: "Pause"},
			{Name: "PlayPause"},
			{Name: "Stop"},
			{Name: "Play"},
			{
				Name: "Seek",
				Args: []introspect.Arg{{Name: "Offset", Type: "x", Direction: "in"}},
			},
			{
				Name: "SetPosition",
				Args: []introspect.Arg{
					{Name: "TrackId", Type: "o", Direction: "in"},
					{Name: "Position", Type: "x", Direction: "in"},
				},
			},
			{
				Name: "OpenUri",
				Args: []introspect.Arg{{Name: "Uri", Type: "s", Direction: "in"}},
			},
		},
		Signals: []introspect.Signal{
			{
				Name: "Seeked",
				Args: []introspect.Arg{{Name: "Position", Type: "x", Direction: "out"}},
			},
		},
		Properties: props.Introspection(mprisPlayerIface),
	}
}

func (m *mprisServer) update(st State) {
	if m == nil || m.props == nil {
		return
	}

	status := mprisPlaybackStatus(st.Status)
	volume := mprisVolume(st.Volume)

	m.mu.Lock()
	defer m.mu.Unlock()

	key := st.StationID + "\x00" + st.Artist + "\x00" + st.Title
	trackID := mprisNoTrack
	metadataChanged := false
	if key != m.trackKey {
		m.trackKey = key
		if st.StationID != "" || st.Artist != "" || st.Title != "" {
			m.trackSeq++
			trackID = dbus.ObjectPath(fmt.Sprintf("/org/priyomnik/track/%d", m.trackSeq))
		}
		metadataChanged = true
	}

	if status != m.lastStatus {
		m.props.SetMust(mprisPlayerIface, "PlaybackStatus", status)
		m.lastStatus = status
	}
	if math.Abs(volume-m.lastVolume) > 0.0001 {
		m.props.SetMust(mprisPlayerIface, "Volume", volume)
		m.lastVolume = volume
	}
	if metadataChanged || !m.lastMetadata {
		m.props.SetMust(mprisPlayerIface, "Metadata", mprisMetadata(st, trackID))
		m.lastMetadata = true
	}
}

func (m *mprisServer) setVolume(c *prop.Change) *dbus.Error {
	v, ok := c.Value.(float64)
	if !ok {
		return prop.ErrInvalidArg
	}
	volume := mprisVolumePercent(v)
	// Do not call back into the property map while godbus holds its lock.
	go func() {
		if _, err := m.app.applyVolume(volume); err != nil {
			slog.Error("mpris: set volume", "err", err)
		}
	}()
	return nil
}

func mprisPlaybackStatus(status Status) string {
	if status == StatusPlaying || status == StatusConnecting {
		return "Playing"
	}
	return "Stopped"
}

func mprisVolume(volume int) float64 {
	return float64(clampVolume(volume)) / 100
}

func mprisVolumePercent(volume float64) int {
	if volume < 0 {
		volume = 0
	}
	if volume > 1 {
		volume = 1
	}
	return clampVolume(int(math.Round(volume * 100)))
}

func mprisMetadata(st State, trackID dbus.ObjectPath) map[string]dbus.Variant {
	meta := map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(trackID),
	}
	title := st.Title
	if title == "" {
		title = st.StationName
	}
	if title != "" {
		meta["xesam:title"] = dbus.MakeVariant(title)
	}
	if st.Artist != "" {
		meta["xesam:artist"] = dbus.MakeVariant([]string{st.Artist})
	}
	if st.StationName != "" {
		meta["xesam:album"] = dbus.MakeVariant(st.StationName)
	}
	return meta
}

// Root interface methods.

func (m *mprisServer) Raise() *dbus.Error {
	return nil
}

func (m *mprisServer) Quit() *dbus.Error {
	go m.app.quit()
	return nil
}

// Player interface methods. Streams are not seekable; Seek and SetPosition are
// accepted as no-ops so clients that send them do not see an error.

func (m *mprisServer) Next() *dbus.Error {
	go func() {
		if err := m.app.step(1); err != nil {
			slog.Error("mpris: next", "err", err)
		}
	}()
	return nil
}

func (m *mprisServer) Previous() *dbus.Error {
	go func() {
		if err := m.app.step(-1); err != nil {
			slog.Error("mpris: previous", "err", err)
		}
	}()
	return nil
}

func (m *mprisServer) Pause() *dbus.Error {
	go func() {
		if err := m.app.player.Stop(); err != nil {
			slog.Error("mpris: pause", "err", err)
		}
	}()
	return nil
}

func (m *mprisServer) PlayPause() *dbus.Error {
	go func() {
		if err := m.app.toggle(); err != nil {
			slog.Error("mpris: play/pause", "err", err)
		}
	}()
	return nil
}

func (m *mprisServer) Stop() *dbus.Error {
	go func() {
		if err := m.app.player.Stop(); err != nil {
			slog.Error("mpris: stop", "err", err)
		}
	}()
	return nil
}

func (m *mprisServer) Play() *dbus.Error {
	go func() {
		if err := m.app.playLast(); err != nil {
			slog.Error("mpris: play", "err", err)
		}
	}()
	return nil
}

func (m *mprisServer) SeekPosition(offset int64) *dbus.Error {
	return nil
}

func (m *mprisServer) SetPosition(trackID dbus.ObjectPath, position int64) *dbus.Error {
	return nil
}

func (m *mprisServer) OpenUri(uri string) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.NotSupported", []interface{}{"OpenUri is not supported"})
}
