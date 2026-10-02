// atcli sends AT commands to the module over the user-space USB transport.
//
//	atcli "AT+CSQ" "AT+QAUDMOD?" ...
//
// Unlike the one-shot ObjC probe (tools/atprobe.m) this keeps the device open
// and drains stale responses between commands, so results are reliable.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/modem"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: atcli \"AT+CSQ\" [more commands...]")
		os.Exit(2)
	}
	m, err := modem.NewUSBATModem()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open module:", err)
		os.Exit(1)
	}
	defer m.Close()

	ctx := context.Background()
	for _, cmd := range os.Args[1:] {
		// AT+CLAC and friends stream a long list; give every command room.
		out, err := m.ATTimeout(ctx, cmd, 30*time.Second)
		switch {
		case err != nil:
			fmt.Printf("%-28s -> ERROR: %v\n", cmd, err)
		default:
			trimmed := strings.TrimRight(out, "\n")
			if trimmed == "" {
				trimmed = "(no data)"
			}
			fmt.Printf("%-28s -> %s\n", cmd, strings.ReplaceAll(trimmed, "\n", " | "))
		}
	}
}
