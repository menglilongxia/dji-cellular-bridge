// qdc507-adb drives the module's ADB channel through the same in-process USB
// transport the gateway uses (IOKit, no libusb, no second process).
//
//	# one shell command
//	qdc507-adb shell "id -u; ls /dev/snd"
//
//	# what the module currently thinks about the voice route
//	qdc507-adb check
//
//	# provision / tear down / revive the module-side voice route
//	qdc507-adb start   [--files DIR]
//	qdc507-adb stop
//	qdc507-adb recover
//
// Only one process can own the USB device: stop the gateway first
// (launchctl bootout gui/$UID/com.cellbridge.gateway) or drive it over HTTP
// instead (POST /api/v1/voice/start).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/modem"
	"github.com/dji-cellular-bridge/mac-gateway/internal/voiceroute"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: qdc507-adb shell \"<command>\" | check | start | stop | recover")
		flag.PrintDefaults()
	}
	files := flag.String("files", defaultFilesDir(), "directory holding the vendor voice runtime")
	moduleIP := flag.String("module-ip", voiceroute.DefaultModuleIP, "module address on the USB ECM link")
	timeout := flag.Duration("timeout", 60*time.Second, "per-command timeout")
	repeat := flag.Int("n", 1, "run the shell command this many times in one session (diagnostics)")
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	m, err := modem.NewUSBATModem()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open module:", err)
		fmt.Fprintln(os.Stderr, "hint: the gateway may still own the device — launchctl bootout gui/$UID/com.cellbridge.gateway")
		os.Exit(1)
	}
	defer m.Close()

	client := m.ADBClient()
	if client == nil {
		fmt.Fprintln(os.Stderr, "this module exposes no ADB interface (check the usbcfg composition)")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	provisioner := voiceroute.New(voiceroute.Options{
		ADB:      client,
		FilesDir: *files,
		ModuleIP: *moduleIP,
		Logf:     func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	})

	switch flag.Arg(0) {
	case "shell":
		if flag.NArg() < 2 {
			fmt.Fprintln(os.Stderr, "shell needs a command")
			os.Exit(2)
		}
		for i := 0; i < *repeat; i++ {
			out, err := client.Shell(ctx, flag.Arg(1), *timeout)
			fmt.Printf("[%d] %s", i+1, out)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%d] shell: %v\n", i+1, err)
				os.Exit(1)
			}
		}

	case "check":
		out, err := provisioner.Check(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "check:", err)
			os.Exit(1)
		}
		fmt.Print(out["raw"])

	case "script":
		// Print (and optionally run) the exact module-side script, so it can be
		// pasted into a shell when the module misbehaves.
		fmt.Print(provisioner.Script())
		if flag.NArg() > 1 && flag.Arg(1) == "run" {
			out, err := client.Shell(ctx, provisioner.Script(), *timeout)
			fmt.Println("--- output ---")
			fmt.Print(out)
			if err != nil {
				fmt.Fprintln(os.Stderr, "script:", err)
				os.Exit(1)
			}
		}

	case "start":
		res, err := provisioner.Start(ctx)
		fmt.Printf("duration=%s pushed=%v modules=%v calibrated=%v bridge=%v\n",
			res.Duration.Round(time.Millisecond), res.Pushed, res.ModulesLoaded, res.Calibrated, res.BridgeRunning)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start:", err)
			os.Exit(1)
		}

	case "stop":
		if err := provisioner.Stop(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "stop:", err)
			os.Exit(1)
		}
		fmt.Println("stopped")

	case "recover":
		if err := m.RecoverADB(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "recover:", err)
			os.Exit(1)
		}
		fmt.Println("usb device re-enumerated")

	default:
		flag.Usage()
		os.Exit(2)
	}
}

func defaultFilesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "module-voice"
	}
	return home + "/Library/Application Support/CellBridge/module-voice"
}
