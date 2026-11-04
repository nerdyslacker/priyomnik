package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `priyomnik - minimal internet radio tray player

Usage:
  priyomnik                 Start tray (or use the running instance)
  priyomnik play [STATION]  Play last or named station
  priyomnik stop            Stop playback
  priyomnik toggle          Toggle playback
  priyomnik next            Play next station
  priyomnik prev            Play previous station
  priyomnik volume [+-]N    Adjust volume
  priyomnik mute            Toggle mute
  priyomnik status [--json] Show status
  priyomnik reload          Reload configuration
  priyomnik quit            Quit the running instance
  priyomnik help            Show this help

Configuration:
  ${XDG_CONFIG_HOME:-$HOME/.config}/priyomnik/config.json
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runTray()
	}

	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "play":
		return cmdPlay(args[1:], stdout, stderr)
	case "stop":
		return cmdNoArgs("stop", args[1:], stdout, stderr)
	case "toggle":
		return cmdNoArgs("toggle", args[1:], stdout, stderr)
	case "next":
		return cmdNoArgs("next", args[1:], stdout, stderr)
	case "prev":
		return cmdNoArgs("prev", args[1:], stdout, stderr)
	case "volume":
		return cmdVolume(args[1:], stdout, stderr)
	case "mute":
		return cmdNoArgs("mute", args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "reload":
		return cmdNoArgs("reload", args[1:], stdout, stderr)
	case "quit":
		return cmdNoArgs("quit", args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "priyomnik: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
