package voiceroute

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/adb"
)

// fakeModule answers ADB shell commands with scripted results and records what
// the provisioner asked for.
type fakeModule struct {
	mu      sync.Mutex
	rx      []byte
	tx      []byte
	shells  []string
	handler func(cmd string) string
}

func newFakeModule(handler func(string) string) *fakeModule {
	return &fakeModule{handler: handler}
}

func (f *fakeModule) Write(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rx = append(f.rx, p...)
	for len(f.rx) >= 24 {
		h := f.rx[:24]
		cmd := binary.LittleEndian.Uint32(h[0:4])
		n := int(binary.LittleEndian.Uint32(h[12:16]))
		if len(f.rx) < 24+n {
			return nil
		}
		payload := string(f.rx[24 : 24+n])
		f.rx = f.rx[24+n:]
		switch cmd {
		case 0x4e584e43: // CNXN
			f.push(adbPacket(0x4e584e43, 0, adb.MaxData, []byte("device::")))
		case 0x4e45504f: // OPEN
			service := strings.TrimRight(payload, "\x00")
			f.push(adbPacket(0x59414b4f, 7, 0, nil)) // OKAY
			cmdline := strings.TrimPrefix(service, "shell:")
			f.shells = append(f.shells, cmdline)
			if out := f.handler(cmdline); out != "" {
				f.push(adbPacket(0x45545257, 7, 0, []byte(out))) // WRTE
			}
			f.push(adbPacket(0x45534c43, 7, 0, nil)) // CLSE
		}
	}
	return nil
}

func (f *fakeModule) push(b []byte) { f.tx = append(f.tx, b...) }

func (f *fakeModule) Read(p []byte, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		if len(f.tx) > 0 {
			n := copy(p, f.tx)
			f.tx = f.tx[n:]
			f.mu.Unlock()
			return n, nil
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			return 0, nil
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fakeModule) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.shells...)
}

func adbPacket(cmd, arg0, arg1 uint32, payload []byte) []byte {
	h := make([]byte, 24)
	binary.LittleEndian.PutUint32(h[0:4], cmd)
	binary.LittleEndian.PutUint32(h[4:8], arg0)
	binary.LittleEndian.PutUint32(h[8:12], arg1)
	binary.LittleEndian.PutUint32(h[12:16], uint32(len(payload)))
	var sum uint32
	for _, b := range payload {
		sum += uint32(b)
	}
	binary.LittleEndian.PutUint32(h[16:20], sum)
	binary.LittleEndian.PutUint32(h[20:24], cmd^0xffffffff)
	return append(h, payload...)
}

// vendorDir writes stand-in runtime files with the real names.
func vendorDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range runtimeFiles {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte("payload of "+f.name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func scriptAnswer(modules, fifo, cal, ready string) string {
	return fmt.Sprintf("MODULES=%s\nMAVO-SURVIVORS=0\n%s\nCAL-COUNT=%s\nROUTE-READY=%s\n"+
		"AUDIO-ENABLE=1\nPCM-PLAY=RUNNING\nPCM-CAP=RUNNING\nPROVISION-DONE\n",
		modules, fifo, cal, ready)
}

func TestStartHappyPath(t *testing.T) {
	mod := newFakeModule(func(string) string { return scriptAnswer("2", "FIFO-OK", "1", "yes") })
	p := New(Options{ADB: adb.New(mod), FilesDir: vendorDir(t), ModuleIP: "127.0.0.1"})

	res, err := p.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !res.ModulesLoaded || !res.Calibrated || !res.BridgeRunning {
		t.Fatalf("flags not set: %+v", res)
	}
	if len(res.Pushed) != 0 {
		t.Fatalf("nothing should have been pushed, got %v", res.Pushed)
	}
	// One shell command for the whole per-call sequence: every ADB shell costs
	// the module a fork, and forks are what it runs out of.
	if got := len(mod.commands()); got != 1 {
		t.Fatalf("want 1 shell command on the happy path, got %d: %v", got, mod.commands())
	}
}

func TestCalibrationFailureIsReported(t *testing.T) {
	mod := newFakeModule(func(cmd string) string {
		if strings.Contains(cmd, "tail -n 3") {
			return "set _verb VoLTE\nERROR\n"
		}
		return scriptAnswer("2", "FIFO-OK", "0", "yes")
	})
	p := New(Options{ADB: adb.New(mod), FilesDir: vendorDir(t), ModuleIP: "127.0.0.1"})

	_, err := p.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "calibration") {
		t.Fatalf("want a calibration error, got %v", err)
	}
}

func TestMissingRuntimeIsPushedThenRetried(t *testing.T) {
	files := vendorDir(t)
	var (
		installed = map[string]string{}
		mu        sync.Mutex
		run       int
	)
	// The provisioner gives each runtime file its own listen port.
	portFile := map[int]string{}
	for i, f := range runtimeFiles {
		portFile[pushPortBase+i] = f.name
	}
	mod := newFakeModule(func(cmd string) string {
		switch {
		case strings.Contains(cmd, "netstat -lnt"):
			return "1\n"
		case strings.Contains(cmd, "busybox md5sum"):
			var b strings.Builder
			for _, line := range strings.Split(cmd, " ") {
				name := strings.TrimPrefix(line, DefaultRemoteDir+"/")
				mu.Lock()
				sum, ok := installed[name]
				mu.Unlock()
				if ok {
					fmt.Fprintf(&b, "%s  %s\n", sum, name)
				}
			}
			if b.Len() == 0 {
				mu.Lock()
				t.Logf("md5 miss for %q (installed=%v)", cmd, installed)
				mu.Unlock()
			}
			return b.String()
		case strings.Contains(cmd, "PROVISION-DONE"): // the provisioning script
			mu.Lock()
			defer mu.Unlock()
			run++
			if run == 1 {
				return "MAVO-DIR-MISSING\n"
			}
			return scriptAnswer("2", "FIFO-OK", "1", "yes")
		}
		return ""
	})

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var port int
		fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &port)
		// The module ends up with exactly the bytes we send, so the digest it
		// would report is the local file's.
		if name, ok := portFile[port]; ok {
			if data, err := os.ReadFile(filepath.Join(files, name)); err == nil {
				sum := md5.Sum(data)
				mu.Lock()
				installed[name] = hex.EncodeToString(sum[:])
				mu.Unlock()
			}
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			buf := make([]byte, 4096)
			for {
				// Stand in for busybox nc: read until the peer goes quiet,
				// then close (which is what tells the sender the file landed).
				_ = server.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				if _, err := server.Read(buf); err != nil {
					return
				}
			}
		}()
		return client, nil
	}

	p := New(Options{ADB: adb.New(mod), FilesDir: files, ModuleIP: "127.0.0.1", Dial: dial})
	res, err := p.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(res.Pushed) != len(runtimeFiles) {
		t.Fatalf("want %d pushed files, got %v", len(runtimeFiles), res.Pushed)
	}
	if run < 2 {
		t.Fatalf("the script should have been retried after installing, ran %d time(s)", run)
	}
}
