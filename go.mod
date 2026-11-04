module priyomnik

go 1.27.3

require (
	fyne.io/systray v0.0.0-20261104133901-be739c8a479d
	github.com/godbus/dbus/v5 v5.1.0
)

require golang.org/x/sys v0.15.0 // indirect

replace fyne.io/systray => github.com/nerdyslacker/systray v0.0.0-20261104133901-be739c8a479d
