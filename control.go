package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type controlRequest struct {
	Command string `json:"command"`
	Station string `json:"station,omitempty"`
	Volume  string `json:"volume,omitempty"`
	Minutes string `json:"minutes,omitempty"`
}

type controlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	State *State `json:"state,omitempty"`
}

type controlServer struct {
	app       *trayApp
	ln        net.Listener
	sock      string
	closeOnce sync.Once
}

var errAlreadyRunning = errors.New("another instance is running")

func controlSocketPath() (string, error) {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		dir := filepath.Join(xdg, "priyomnik")
		sock := filepath.Join(dir, "control.sock")
		if len(sock) <= 100 {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
			if err := secureDir(dir); err != nil {
				return "", err
			}
			return sock, nil
		}
	}

	dir := filepath.Join(os.TempDir(), fmt.Sprintf("priyomnik-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := secureDir(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, "control.sock"), nil
}

func acquireControlLock(sock string) (*os.File, error) {
	path := filepath.Join(filepath.Dir(sock), "control.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errAlreadyRunning
		}
		return nil, err
	}
	return f, nil
}

func (a *trayApp) startControl() error {
	sock := a.controlSock
	if sock == "" {
		return errors.New("control socket path is not set")
	}
	if err := removeStaleSocket(sock); err != nil {
		return err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		os.Remove(sock)
		return err
	}

	srv := &controlServer{app: a, ln: ln, sock: sock}
	a.mu.Lock()
	a.control = srv
	a.mu.Unlock()
	go srv.serve()
	return nil
}

func (a *trayApp) closeControl() {
	a.mu.Lock()
	srv := a.control
	a.control = nil
	a.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

func (s *controlServer) Close() {
	s.closeOnce.Do(func() {
		_ = s.ln.Close()
		_ = os.Remove(s.sock)
	})
}

func (s *controlServer) serve() {
	sem := make(chan struct{}, 8)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("control accept", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			s.handle(conn)
		}()
	}
}

func (s *controlServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := checkPeerUID(conn); err != nil {
		slog.Warn("control peer rejected", "err", err)
		writeControlResponse(conn, controlResponse{Error: "unauthorized client"})
		return
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	if !scanner.Scan() {
		msg := "empty request"
		if scanner.Err() != nil {
			msg = "invalid request"
		}
		writeControlResponse(conn, controlResponse{Error: msg})
		return
	}

	var req controlRequest
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		writeControlResponse(conn, controlResponse{Error: "invalid request"})
		return
	}

	resp, quit := s.app.handleControl(req)
	writeControlResponse(conn, resp)
	if quit {
		s.app.quit()
	}
}

func writeControlResponse(conn net.Conn, resp controlResponse) {
	_ = json.NewEncoder(conn).Encode(resp)
}

func checkPeerUID(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}

	var ucred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		ucred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if int(ucred.Uid) != os.Getuid() {
		return errors.New("peer uid mismatch")
	}
	return nil
}

func (a *trayApp) handleControl(req controlRequest) (controlResponse, bool) {
	var err error
	quit := false

	switch req.Command {
	case "play":
		if req.Station == "" {
			err = a.playLast()
		} else {
			err = a.playID(req.Station)
		}
	case "stop":
		err = a.player.Stop()
	case "toggle":
		err = a.toggle()
	case "next":
		err = a.step(1)
	case "prev":
		err = a.step(-1)
	case "volume":
		_, err = a.controlVolume(req.Volume)
	case "mute":
		_, err = a.player.ToggleMute()
	case "sleep":
		minutes, perr := parseSleepMinutes(req.Minutes)
		if perr != nil {
			err = perr
		} else {
			err = a.setSleepTimer(minutes)
		}
	case "status":
	case "reload":
		err = a.reload()
	case "quit":
		quit = true
	default:
		err = fmt.Errorf("unknown command %q", req.Command)
	}

	if err != nil {
		return controlResponse{Error: err.Error()}, false
	}
	st := a.statusState()
	return controlResponse{OK: true, State: &st}, quit
}

func (a *trayApp) statusState() State {
	st := a.player.Snapshot()
	a.mu.RLock()
	selected := a.selected
	a.mu.RUnlock()
	if st.StationID == "" {
		st.StationID = selected
	}
	if st.StationName == "" && st.StationID != "" {
		cfg := a.copyConfig()
		if station, ok := findStation(cfg.Stations, st.StationID); ok {
			st.StationName = station.Name
		}
	}
	return st
}

func (a *trayApp) playLast() error {
	cfg := a.copyConfig()
	if cfg.LastStation == "" {
		return errors.New("no station selected")
	}
	return a.playID(cfg.LastStation)
}

func (a *trayApp) toggle() error {
	st := a.player.Snapshot()
	if st.Status == StatusPlaying || st.Status == StatusConnecting {
		return a.player.Stop()
	}
	return a.playLast()
}

func (a *trayApp) step(delta int) error {
	cfg := a.copyConfig()
	if len(cfg.Stations) == 0 {
		return errors.New("no stations configured")
	}

	id := a.player.Snapshot().StationID
	if id == "" {
		a.mu.RLock()
		id = a.selected
		a.mu.RUnlock()
	}

	idx := -1
	for i, st := range cfg.Stations {
		if st.ID == id {
			idx = i
			break
		}
	}
	switch {
	case idx < 0 && delta > 0:
		idx = 0
	case idx < 0:
		idx = len(cfg.Stations) - 1
	default:
		idx = (idx + delta + len(cfg.Stations)) % len(cfg.Stations)
	}
	return a.playStation(cfg.Stations[idx])
}

func (a *trayApp) controlVolume(raw string) (int, error) {
	if raw == "" {
		return 0, errors.New("volume requires a value")
	}
	delta, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid volume %q", raw)
	}
	if raw[0] == '+' || raw[0] == '-' {
		return a.applyVolume(a.player.Snapshot().Volume + delta)
	}
	return a.applyVolume(delta)
}

func (a *trayApp) applyVolume(v int) (int, error) {
	v, err := a.player.SetVolume(v)
	if err != nil {
		return v, err
	}

	a.mu.Lock()
	changed := a.cfg.Volume != v
	a.mu.Unlock()
	if changed {
		a.updateConfig(func(c *Config) {
			c.Volume = v
		})
	}
	return v, nil
}

func cmdPlay(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		return cliUsage(stderr, "play [STATION]")
	}
	req := controlRequest{Command: "play"}
	if len(args) == 1 {
		req.Station = args[0]
	}
	return sendControl(req, false, stdout, stderr)
}

func cmdVolume(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return cliUsage(stderr, "volume [+-]N")
	}
	return sendControl(controlRequest{Command: "volume", Volume: args[0]}, false, stdout, stderr)
}

func cmdSleep(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return cliUsage(stderr, "sleep N|off")
	}
	return sendControl(controlRequest{Command: "sleep", Minutes: args[0]}, false, stdout, stderr)
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	asJSON := false
	switch {
	case len(args) == 1 && args[0] == "--json":
		asJSON = true
	case len(args) != 0:
		return cliUsage(stderr, "status [--json]")
	}
	return sendControl(controlRequest{Command: "status"}, asJSON, stdout, stderr)
}

func cmdNoArgs(command string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		return cliUsage(stderr, command)
	}
	return sendControl(controlRequest{Command: command}, false, stdout, stderr)
}

func cliUsage(stderr io.Writer, usage string) int {
	fmt.Fprintf(stderr, "priyomnik: usage: priyomnik %s\n", usage)
	return 2
}

func sendControl(req controlRequest, jsonStatus bool, stdout, stderr io.Writer) int {
	st, err := controlCall(req)
	if err != nil {
		fmt.Fprintf(stderr, "priyomnik: %v\n", err)
		return 1
	}
	if req.Command != "status" || st == nil {
		return 0
	}

	if jsonStatus {
		data, err := json.Marshal(st)
		if err != nil {
			fmt.Fprintf(stderr, "priyomnik: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}

	id := st.StationID
	if id == "" {
		id = "-"
	}
	fmt.Fprintf(stdout, "%s %s %d\n", st.Status, id, st.Volume)
	return 0
}

func controlCall(req controlRequest) (*State, error) {
	sock, err := controlSocketPath()
	if err != nil {
		return nil, err
	}

	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) ||
			errors.Is(err, syscall.ECONNREFUSED) ||
			errors.Is(err, syscall.ECONNRESET) {
			return nil, errors.New("no running instance")
		}
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp controlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "request failed"
		}
		return nil, errors.New(resp.Error)
	}
	return resp.State, nil
}
