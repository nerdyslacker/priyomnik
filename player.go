package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
)

type Status string

const (
	StatusStopped    Status = "stopped"
	StatusConnecting Status = "connecting"
	StatusPlaying    Status = "playing"
	StatusError      Status = "error"
)

const (
	maxRetries  = 3
	retryBase   = time.Second
	retryMax    = 8 * time.Second
	ipcMaxLine  = 1 << 20
	metadataMax = 200
)

type State struct {
	Status      Status `json:"status"`
	StationID   string `json:"station_id,omitempty"`
	StationName string `json:"station_name,omitempty"`
	Artist      string `json:"artist,omitempty"`
	Title       string `json:"title,omitempty"`
	Volume      int    `json:"volume"`
	Mute        bool   `json:"mute"`
	Error       string `json:"error,omitempty"`
}

type Player struct {
	mu         sync.Mutex
	status     Status
	station    Station
	artist     string
	title      string
	mediaTitle string
	meta       map[string]any
	volume     int
	mute       bool
	errMsg     string
	loaded     bool
	buffering  bool
	userActive bool
	gen        uint64
	attempt    int
	retryStop  context.CancelFunc
	closing    bool
	updates    chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	startMu sync.Mutex
	opMu    sync.Mutex
	running atomic.Bool
	cmd     *exec.Cmd
	cmdDone chan struct{}
	ipc     *ipcConn
	dir     string
	sock    string
	ownDir  bool
}

func NewPlayer(volume int) *Player {
	volume = clampVolume(volume)
	ctx, cancel := context.WithCancel(context.Background())
	return &Player{
		status:  StatusStopped,
		volume:  volume,
		updates: make(chan struct{}, 1),
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (p *Player) Updates() <-chan struct{} {
	return p.updates
}

func (p *Player) Snapshot() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return State{
		Status:      p.status,
		StationID:   p.station.ID,
		StationName: p.station.Name,
		Artist:      p.artist,
		Title:       p.title,
		Volume:      p.volume,
		Mute:        p.mute,
		Error:       p.errMsg,
	}
}

func (p *Player) Play(st Station) error {
	if strings.TrimSpace(st.URL) == "" {
		return errors.New("station has no URL")
	}

	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return errors.New("player is closed")
	}
	p.gen++
	gen := p.gen
	p.station = st
	p.artist, p.title, p.mediaTitle = "", "", ""
	p.meta = nil
	p.loaded = false
	p.buffering = false
	p.status = StatusConnecting
	p.errMsg = ""
	p.attempt = 0
	p.userActive = true
	p.cancelRetryLocked()
	p.mu.Unlock()
	p.notify()

	if err := p.ensureStarted(); err != nil {
		p.setError(err)
		return err
	}
	if err := p.load(gen, st.URL); err != nil {
		if p.genCurrent(gen) {
			p.streamFailed(err.Error())
		}
		return err
	}
	return nil
}

func (p *Player) Stop() error {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil
	}
	p.gen++
	p.userActive = false
	p.cancelRetryLocked()
	p.status = StatusStopped
	p.errMsg = ""
	p.artist, p.title, p.mediaTitle = "", "", ""
	p.meta = nil
	p.loaded = false
	p.buffering = false
	p.mu.Unlock()
	p.notify()

	if !p.running.Load() {
		return nil
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	_, err := p.command(3*time.Second, "stop")
	if err != nil && !errors.Is(err, errIPCClosed) {
		return err
	}
	return nil
}

func (p *Player) Toggle() error {
	p.mu.Lock()
	active := p.status == StatusPlaying || p.status == StatusConnecting
	st := p.station
	p.mu.Unlock()

	if active {
		return p.Stop()
	}
	if strings.TrimSpace(st.URL) == "" {
		return errors.New("no station selected")
	}
	return p.Play(st)
}

func (p *Player) SetVolume(v int) (int, error) {
	v = clampVolume(v)
	if p.running.Load() {
		if _, err := p.command(2*time.Second, "set_property", "volume", v); err != nil {
			return p.Snapshot().Volume, err
		}
	}
	p.mu.Lock()
	changed := p.volume != v
	p.volume = v
	p.mu.Unlock()
	if changed {
		p.notify()
	}
	return v, nil
}

func (p *Player) AdjustVolume(delta int) (int, error) {
	p.mu.Lock()
	v := p.volume
	p.mu.Unlock()
	return p.SetVolume(v + delta)
}

func (p *Player) ToggleMute() (bool, error) {
	p.mu.Lock()
	target := !p.mute
	p.mu.Unlock()

	if p.running.Load() {
		if _, err := p.command(2*time.Second, "set_property", "mute", target); err != nil {
			return p.Snapshot().Mute, err
		}
	}

	p.mu.Lock()
	p.mute = target
	p.mu.Unlock()
	p.notify()
	return target, nil
}

func (p *Player) Close() error {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil
	}
	p.closing = true
	p.gen++
	p.userActive = false
	p.cancelRetryLocked()
	p.status = StatusStopped
	p.artist, p.title, p.mediaTitle = "", "", ""
	p.meta = nil
	ipc := p.ipc
	cmd := p.cmd
	done := p.cmdDone
	dir, sock, own := p.dir, p.sock, p.ownDir
	p.mu.Unlock()
	p.notify()

	p.startMu.Lock()
	defer p.startMu.Unlock()

	if ipc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = ipc.call(ctx, "quit")
		cancel()
		ipc.close()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
		}
	}

	if sock != "" {
		_ = os.Remove(sock)
	}
	if own && dir != "" {
		_ = os.RemoveAll(dir)
	}
	p.cancel()

	p.mu.Lock()
	p.ipc = nil
	p.cmd = nil
	p.cmdDone = nil
	p.dir, p.sock, p.ownDir = "", "", false
	p.running.Store(false)
	p.mu.Unlock()
	return nil
}

func (p *Player) load(gen uint64, url string) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()

	p.mu.Lock()
	ok := p.gen == gen && p.userActive && !p.closing
	if ok {
		p.status = StatusConnecting
		p.errMsg = ""
		p.loaded = false
		p.buffering = false
	}
	p.mu.Unlock()
	if !ok {
		return nil
	}
	p.notify()

	_, _ = p.command(3*time.Second, "stop")

	p.mu.Lock()
	ok = p.gen == gen && p.userActive && !p.closing
	p.mu.Unlock()
	if !ok {
		return nil
	}

	_, err := p.command(5*time.Second, "loadfile", url)
	return err
}

func (p *Player) genCurrent(gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen == gen && p.userActive && !p.closing
}

func (p *Player) setError(err error) {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return
	}
	p.status = StatusError
	p.errMsg = cleanText(err.Error())
	p.userActive = false
	p.cancelRetryLocked()
	p.mu.Unlock()
	p.notify()
}

func (p *Player) streamFailed(msg string) {
	p.mu.Lock()
	if p.closing || !p.userActive || p.status == StatusStopped || p.status == StatusError {
		p.mu.Unlock()
		return
	}

	msg = cleanText(msg)
	if msg == "" {
		msg = "stream failed"
	}
	p.attempt++
	if p.attempt > maxRetries {
		p.status = StatusError
		p.errMsg = msg
		p.userActive = false
		p.cancelRetryLocked()
		p.mu.Unlock()
		p.notify()
		return
	}

	p.status = StatusError
	p.errMsg = msg
	gen := p.gen
	station := p.station
	delay := retryDelay(p.attempt)
	p.cancelRetryLocked()
	ctx, cancel := context.WithCancel(p.ctx)
	p.retryStop = cancel
	p.mu.Unlock()
	p.notify()

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}

		p.mu.Lock()
		if p.gen != gen || !p.userActive || p.closing || p.status != StatusError {
			p.mu.Unlock()
			return
		}
		p.status = StatusConnecting
		p.errMsg = ""
		p.mu.Unlock()
		p.notify()

		if err := p.ensureStarted(); err != nil {
			if p.genCurrent(gen) {
				p.streamFailed(err.Error())
			}
			return
		}
		if err := p.load(gen, station.URL); err != nil && p.genCurrent(gen) {
			p.streamFailed(err.Error())
		}
	}()
}

func retryDelay(attempt int) time.Duration {
	d := retryBase << (attempt - 1)
	if d > retryMax {
		d = retryMax
	}
	return d
}

func (p *Player) cancelRetryLocked() {
	if p.retryStop != nil {
		p.retryStop()
		p.retryStop = nil
	}
}

func (p *Player) notify() {
	select {
	case p.updates <- struct{}{}:
	default:
	}
}

func (p *Player) ensureStarted() error {
	p.startMu.Lock()
	defer p.startMu.Unlock()

	if p.running.Load() {
		return nil
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return errors.New("player is closed")
	}
	volume, mute := p.volume, p.mute
	p.mu.Unlock()

	dir, own, sock, err := playerSocketPath()
	if err != nil {
		return err
	}
	if err := removeStaleSocket(sock); err != nil {
		if own {
			_ = os.RemoveAll(dir)
		}
		return err
	}

	cmd := exec.Command("mpv",
		"--no-video",
		"--no-terminal",
		"--idle=yes",
		"--input-ipc-server="+sock,
		"--volume="+strconv.Itoa(volume),
	)
	if mute {
		cmd.Args = append(cmd.Args, "--mute=yes")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(sock)
		if own {
			_ = os.RemoveAll(dir)
		}
		return fmt.Errorf("start mpv: %w", err)
	}

	done := make(chan struct{})
	p.mu.Lock()
	p.cmd = cmd
	p.cmdDone = done
	p.dir = dir
	p.sock = sock
	p.ownDir = own
	p.mu.Unlock()
	go p.waitProcess(cmd, done)

	conn, err := dialIPC(sock, done)
	if err != nil {
		return p.abortStart(cmd, done, sock, dir, own, nil, fmt.Errorf("mpv IPC: %w", err))
	}
	ipc := newIPCConn(conn, p.handleEvent)

	for i, name := range []string{"volume", "mute", "metadata", "media-title", "paused-for-cache"} {
		ctx, cancel := context.WithTimeout(p.ctx, 2*time.Second)
		_, err := ipc.call(ctx, "observe_property", i+1, name)
		cancel()
		if err != nil {
			return p.abortStart(cmd, done, sock, dir, own, ipc, fmt.Errorf("observe %s: %w", name, err))
		}
	}

	p.mu.Lock()
	p.ipc = ipc
	p.running.Store(true)
	p.mu.Unlock()
	return nil
}

func (p *Player) abortStart(cmd *exec.Cmd, done chan struct{}, sock, dir string, own bool, ipc *ipcConn, cause error) error {
	if ipc != nil {
		ipc.close()
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-done
	_ = os.Remove(sock)
	if own {
		_ = os.RemoveAll(dir)
	}

	p.mu.Lock()
	if p.cmd == cmd {
		p.cmd = nil
		p.cmdDone = nil
		p.dir, p.sock, p.ownDir = "", "", false
		p.running.Store(false)
	}
	p.mu.Unlock()
	return cause
}

func (p *Player) waitProcess(cmd *exec.Cmd, done chan struct{}) {
	err := cmd.Wait()

	p.mu.Lock()
	same := p.cmd == cmd
	closing := p.closing
	if same {
		p.running.Store(false)
		if !closing && p.status != StatusStopped {
			msg := "mpv exited"
			if err != nil {
				msg = "mpv exited: " + err.Error()
			}
			p.status = StatusError
			p.errMsg = cleanText(msg)
			p.userActive = false
			p.cancelRetryLocked()
		}
	}
	p.mu.Unlock()
	close(done)
	if same {
		p.notify()
	}
}

func (p *Player) command(timeout time.Duration, args ...any) (json.RawMessage, error) {
	p.mu.Lock()
	ipc := p.ipc
	p.mu.Unlock()
	if ipc == nil {
		return nil, errors.New("mpv is not running")
	}

	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()
	return ipc.call(ctx, args...)
}

func (p *Player) handleEvent(msg ipcMessage) {
	switch msg.Event {
	case "start-file":
		p.mu.Lock()
		changed := false
		if p.userActive && !p.closing && p.status != StatusError {
			if p.status != StatusConnecting {
				p.status = StatusConnecting
				changed = true
			}
			p.loaded = false
			p.buffering = false
			if p.errMsg != "" {
				p.errMsg = ""
				changed = true
			}
		}
		p.mu.Unlock()
		if changed {
			p.notify()
		}

	case "file-loaded", "playback-restart":
		p.mu.Lock()
		changed := false
		if p.userActive && !p.closing && p.status != StatusError {
			p.loaded = true
			if !p.buffering {
				if p.status != StatusPlaying {
					p.status = StatusPlaying
					changed = true
				}
				if p.errMsg != "" {
					p.errMsg = ""
					changed = true
				}
			}
		}
		p.mu.Unlock()
		if changed {
			p.notify()
		}

	case "end-file":
		switch msg.Reason {
		case "error":
			text := msg.Error
			if text == "" {
				text = "stream failed"
			}
			p.streamFailed(text)
		case "eof":
			p.streamFailed("stream ended")
		}

	case "property-change":
		p.handleProperty(msg)
	}
}

func (p *Player) handleProperty(msg ipcMessage) {
	switch msg.Name {
	case "volume":
		var f float64
		if json.Unmarshal(msg.Data, &f) != nil {
			return
		}
		v := clampVolume(int(math.Round(f)))
		p.mu.Lock()
		changed := p.volume != v
		if changed {
			p.volume = v
		}
		p.mu.Unlock()
		if changed {
			p.notify()
		}

	case "mute":
		var b bool
		if json.Unmarshal(msg.Data, &b) != nil {
			return
		}
		p.mu.Lock()
		changed := p.mute != b
		if changed {
			p.mute = b
		}
		p.mu.Unlock()
		if changed {
			p.notify()
		}

	case "metadata":
		var m map[string]any
		if json.Unmarshal(msg.Data, &m) != nil {
			return
		}
		p.mu.Lock()
		if !p.userActive || p.closing {
			p.mu.Unlock()
			return
		}
		p.meta = m
		changed := p.refreshMetadataLocked()
		p.mu.Unlock()
		if changed {
			p.notify()
		}

	case "media-title":
		var s string
		if json.Unmarshal(msg.Data, &s) != nil {
			return
		}
		p.mu.Lock()
		if !p.userActive || p.closing {
			p.mu.Unlock()
			return
		}
		p.mediaTitle = s
		changed := p.refreshMetadataLocked()
		p.mu.Unlock()
		if changed {
			p.notify()
		}

	case "paused-for-cache":
		var b bool
		if json.Unmarshal(msg.Data, &b) != nil {
			return
		}
		p.mu.Lock()
		changed := false
		if p.buffering != b {
			p.buffering = b
			changed = true
		}
		if p.userActive && !p.closing {
			if b && p.status == StatusPlaying {
				p.status = StatusConnecting
				changed = true
			} else if !b && p.loaded && p.status == StatusConnecting {
				p.status = StatusPlaying
				changed = true
			}
		}
		p.mu.Unlock()
		if changed {
			p.notify()
		}
	}
}

func (p *Player) refreshMetadataLocked() bool {
	lower := make(map[string]string, len(p.meta))
	for k, v := range p.meta {
		if s, ok := v.(string); ok {
			lower[strings.ToLower(k)] = s
		}
	}

	artist := cleanText(lower["artist"])
	title := cleanText(lower["title"])
	if title == "" {
		title = cleanText(lower["icy-title"])
	}
	if title == "" {
		title = cleanText(p.mediaTitle)
	}
	if artist == "" && title != "" {
		if a, t, ok := strings.Cut(title, " - "); ok {
			if t = cleanText(t); t != "" {
				artist, title = cleanText(a), t
			}
		}
	}
	if isPlaceholderTitle(title, p.station) {
		title = ""
	}

	if p.artist == artist && p.title == title {
		return false
	}
	p.artist, p.title = artist, title
	return true
}

func isPlaceholderTitle(title string, st Station) bool {
	if title == "" {
		return false
	}
	if title == st.Name {
		return true
	}
	u, err := url.Parse(st.URL)
	if err != nil {
		return false
	}
	base := path.Base(u.Path)
	return base != "." && base != "/" && title == base
}

func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > metadataMax {
		s = string(r[:metadataMax])
	}
	return s
}

func clampVolume(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func playerSocketPath() (dir string, own bool, sock string, err error) {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		dir = filepath.Join(xdg, "priyomnik")
		sock = filepath.Join(dir, "mpv.sock")
		if len(sock) <= 100 {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", false, "", err
			}
			if err := secureDir(dir); err != nil {
				return "", false, "", err
			}
			return dir, false, sock, nil
		}
	}

	dir, err = os.MkdirTemp("", "priyomnik-")
	if err != nil {
		return "", false, "", err
	}
	return dir, true, filepath.Join(dir, "mpv.sock"), nil
}

func secureDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a plain directory", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by another user", dir)
	}
	return os.Chmod(dir, 0o700)
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	return os.Remove(path)
}

func dialIPC(sock string, done <-chan struct{}) (net.Conn, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", sock, 300*time.Millisecond)
		if err == nil {
			return conn, nil
		}
		select {
		case <-done:
			return nil, errors.New("mpv exited before its IPC socket was ready")
		default:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect to %s: %w", sock, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var errIPCClosed = errors.New("mpv connection closed")

type ipcResult struct {
	data json.RawMessage
	err  error
}

type ipcMessage struct {
	Event     string          `json:"event"`
	Name      string          `json:"name"`
	RequestID uint64          `json:"request_id"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
	Reason    string          `json:"reason"`
}

type ipcConn struct {
	conn      net.Conn
	writeMu   sync.Mutex
	mu        sync.Mutex
	nextID    uint64
	pending   map[uint64]chan ipcResult
	done      chan struct{}
	closeOnce sync.Once
}

func newIPCConn(conn net.Conn, onEvent func(ipcMessage)) *ipcConn {
	c := &ipcConn{
		conn:    conn,
		pending: make(map[uint64]chan ipcResult),
		done:    make(chan struct{}),
	}
	go c.readLoop(onEvent)
	return c
}

func (c *ipcConn) readLoop(onEvent func(ipcMessage)) {
	defer c.close()

	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 0, 64*1024), ipcMaxLine)
	for scanner.Scan() {
		var msg ipcMessage
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			continue
		}

		if msg.RequestID != 0 {
			c.mu.Lock()
			ch := c.pending[msg.RequestID]
			delete(c.pending, msg.RequestID)
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			if msg.Error != "" && msg.Error != "success" {
				ch <- ipcResult{err: fmt.Errorf("mpv: %s", msg.Error)}
			} else {
				ch <- ipcResult{data: msg.Data}
			}
			continue
		}

		if msg.Event != "" && onEvent != nil {
			onEvent(msg)
		}
	}
}

func (c *ipcConn) call(ctx context.Context, args ...any) (json.RawMessage, error) {
	req := struct {
		Command   []any  `json:"command"`
		RequestID uint64 `json:"request_id"`
	}{Command: args}

	c.mu.Lock()
	c.nextID++
	if c.nextID == 0 {
		c.nextID = 1
	}
	req.RequestID = c.nextID
	ch := make(chan ipcResult, 1)
	c.pending[req.RequestID] = ch
	c.mu.Unlock()

	line, err := json.Marshal(req)
	if err != nil {
		c.forget(req.RequestID)
		return nil, err
	}
	line = append(line, '\n')

	c.writeMu.Lock()
	_, err = c.conn.Write(line)
	c.writeMu.Unlock()
	if err != nil {
		c.forget(req.RequestID)
		return nil, err
	}

	select {
	case res := <-ch:
		return res.data, res.err
	case <-ctx.Done():
		c.forget(req.RequestID)
		return nil, ctx.Err()
	case <-c.done:
		c.forget(req.RequestID)
		return nil, errIPCClosed
	}
}

func (c *ipcConn) forget(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *ipcConn) close() {
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
		close(c.done)
	})
}
