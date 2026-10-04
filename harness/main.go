// harness drives a terminal program in a pty with a fixed key script and
// reports, per step, the bytes it wrote and how long until it went quiet.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

type step struct {
	Name     string
	Keys     string
	Repeat   int
	Bytes    int
	FirstMS  []float64
	LastMS   []float64
}

var mu sync.Mutex
var total int
var lastByte time.Time
var firstAfter time.Time
var marker time.Time

func main() {
	cols := flag.Int("cols", 100, "")
	rows := flag.Int("rows", 30, "")
	quiet := flag.Duration("quiet", 80*time.Millisecond, "output idle this long ends a step")
	out := flag.String("json", "", "write results here")
	idle := flag.Duration("idle", 10*time.Second, "idle CPU window")
	screensOut := flag.String("screens", "", "write the screen after each step here")
	flag.Parse()
	args := flag.Args()

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	start := time.Now()
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(*cols), Rows: uint16(*rows)})
	if err != nil {
		panic(err)
	}
	emu := vt.NewEmulator(*cols, *rows)
	go func() { _, _ = io.Copy(f, emu) }() // the terminal's answers to queries
	screens := map[string]string{}
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := f.Read(buf)
			now := time.Now()
			mu.Lock()
			if n > 0 {
				_, _ = emu.Write(buf[:n])
				total += n
				lastByte = now
				if firstAfter.IsZero() && now.After(marker) {
					firstAfter = now
				}
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	waitQuiet := func(d time.Duration) {
		for {
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			lb := lastByte
			mu.Unlock()
			if !lb.IsZero() && time.Since(lb) > d {
				return
			}
		}
	}
	waitQuiet(500 * time.Millisecond)
	mu.Lock()
	startup := lastByte.Sub(start)
	startBytes := total
	mu.Unlock()

	script := []step{
		{Name: "list down", Keys: "j", Repeat: 36},
		{Name: "list up", Keys: "k", Repeat: 5},
		{Name: "list down again", Keys: "j", Repeat: 5},
		{Name: "open article", Keys: "\r", Repeat: 1},
		{Name: "article scroll", Keys: "j", Repeat: 30},
		{Name: "article page down", Keys: " ", Repeat: 3},
		{Name: "article end", Keys: "G", Repeat: 1},
		{Name: "article top", Keys: "g", Repeat: 1},
		{Name: "next article", Keys: "l", Repeat: 3},
		{Name: "back to list", Keys: "\x1b", Repeat: 1},
		{Name: "filter open", Keys: "/", Repeat: 1},
		{Name: "filter chars", Keys: "go", Repeat: 1},
		{Name: "filter apply", Keys: "\r", Repeat: 1},
		{Name: "filter clear", Keys: "\x1b", Repeat: 1},
	}
	for i := range script {
		s := &script[i]
		for r := 0; r < s.Repeat; r++ {
			for _, k := range splitKeys(s.Keys) {
				mu.Lock()
				before := total
				marker = time.Now()
				firstAfter = time.Time{}
				mu.Unlock()
				sent := time.Now()
				f.Write([]byte(k))
				// Wait for the answer to start (a slow render can take longer
				// than the quiet gap), then for it to end.
				for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); {
					mu.Lock()
					started := !firstAfter.IsZero()
					mu.Unlock()
					if started {
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
				waitQuiet(*quiet)
				mu.Lock()
				s.Bytes += total - before
				if !firstAfter.IsZero() {
					s.FirstMS = append(s.FirstMS, ms(firstAfter.Sub(sent)))
					s.LastMS = append(s.LastMS, ms(lastByte.Sub(sent)))
				}
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
			}
		}
		mu.Lock()
		screens[s.Name] = emu.String()
		mu.Unlock()
	}
	pid := cmd.Process.Pid
	cpu0 := cpuTicks(pid)
	time.Sleep(*idle)
	cpu1 := cpuTicks(pid)
	hwm := statusKB(pid, "VmHWM")
	rss := statusKB(pid, "VmRSS")

	f.Write([]byte("\x03"))
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
	}

	res := map[string]any{
		"startup_ms": ms(startup), "startup_bytes": startBytes,
		"idle_cpu_ms": (cpu1 - cpu0) * 10, // clock ticks are 10 ms
		"peak_rss_kb": hwm, "rss_kb": rss, "steps": script,
	}
	fmt.Printf("startup %.0f ms, %d bytes\n", ms(startup), startBytes)
	fmt.Printf("%-18s %8s %10s %10s %10s\n", "step", "bytes", "first p50", "last p50", "last max")
	for _, s := range script {
		fmt.Printf("%-18s %8d %10.1f %10.1f %10.1f\n", s.Name, s.Bytes, pct(s.FirstMS, 50), pct(s.LastMS, 50), pct(s.LastMS, 100))
	}
	fmt.Printf("idle CPU %d ms in %s; peak RSS %d kB, RSS %d kB\n", (cpu1-cpu0)*10, *idle, hwm, rss)
	if *screensOut != "" {
		var b strings.Builder
		for _, s := range script {
			fmt.Fprintf(&b, "===== after %s\n%s\n", s.Name, screens[s.Name])
		}
		os.WriteFile(*screensOut, []byte(b.String()), 0o644)
	}
	if *out != "" {
		b, _ := json.MarshalIndent(res, "", " ")
		os.WriteFile(*out, b, 0o644)
	}
}

func splitKeys(s string) []string {
	if strings.HasPrefix(s, "\x1b") || len(s) == 1 {
		return []string{s}
	}
	var out []string
	for _, r := range s {
		out = append(out, string(r))
	}
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func pct(v []float64, p int) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	i := (len(c) - 1) * p / 100
	return c[i]
}

func cpuTicks(pid int) int {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	s := string(b)
	s = s[strings.LastIndex(s, ")")+2:]
	f := strings.Fields(s)
	u, _ := strconv.Atoi(f[11])
	st, _ := strconv.Atoi(f[12])
	return u + st
}

func statusKB(pid int, key string) int {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, key+":") {
			n, _ := strconv.Atoi(strings.Fields(l)[1])
			return n
		}
	}
	return 0
}
