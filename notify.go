package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

type notifier struct {
	mu        sync.Mutex
	pending   State
	lastKey   string
	lastID    uint32
	wake      chan struct{}
	stop      chan struct{}
	closeOnce sync.Once
}

func newNotifier() *notifier {
	n := &notifier{
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
	}
	go n.run()
	return n
}

func (n *notifier) enqueue(st State) {
	key := st.StationID + "\x00" + st.Artist + "\x00" + st.Title
	n.mu.Lock()
	if key == n.lastKey {
		n.mu.Unlock()
		return
	}
	n.lastKey = key
	n.pending = st
	n.mu.Unlock()

	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *notifier) Close() {
	n.closeOnce.Do(func() {
		close(n.stop)
	})
}

func (n *notifier) run() {
	for {
		select {
		case <-n.stop:
			return
		case <-n.wake:
			n.mu.Lock()
			st := n.pending
			n.mu.Unlock()
			n.send(st)
		}
	}
}

func (n *notifier) send(st State) {
	summary, body := notificationText(st)
	conn, err := dbus.SessionBus()
	if err != nil {
		slog.Debug("notification bus", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	call := obj.CallWithContext(ctx, "org.freedesktop.Notifications.Notify", 0,
		"Priyomnik", n.lastID, "", summary, body,
		[]string{}, map[string]dbus.Variant{}, int32(5000))
	if call.Err != nil {
		slog.Debug("notification", "err", call.Err)
		return
	}

	var id uint32
	if err := call.Store(&id); err != nil {
		slog.Debug("notification id", "err", err)
		return
	}
	n.lastID = id
}

func notificationText(st State) (string, string) {
	summary := st.StationName
	body := "Playing"
	switch {
	case st.Artist != "" && st.Title != "":
		summary = st.Artist + " — " + st.Title
		body = st.StationName
	case st.Title != "":
		summary = st.Title
		body = st.StationName
	}
	if summary == "" {
		summary = "Priyomnik"
	}
	if body == "" {
		body = "Playing"
	}
	return cleanText(summary), cleanText(body)
}
