package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultURL      = "https://status.duolingo.com/api/v2/summary.json"
	defaultInterval = 30 * time.Second
	httpTimeout     = 15 * time.Second
	inhibitEnv      = "DUOLINGO_WATCHER_INHIBITED"

	soundComplete = "/usr/share/sounds/freedesktop/stereo/complete.oga"
	soundAlarm    = "/usr/share/sounds/freedesktop/stereo/alarm-clock-elapsed.oga"
)

type summaryResponse struct {
	Page       pageInfo      `json:"page"`
	Status     statusInfo    `json:"status"`
	Components []component   `json:"components"`
	Incidents  []incident    `json:"incidents"`
	Scheduled  []maintenance `json:"scheduled_maintenances"`
}

type pageInfo struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	UpdatedAt string `json:"updated_at"`
}

type statusInfo struct {
	Indicator   string `json:"indicator"`
	Description string `json:"description"`
}

type component struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type incident struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Impact  string `json:"impact"`
	Updates []struct {
		Status string `json:"status"`
		Body   string `json:"body"`
	} `json:"incident_updates"`
}

type maintenance struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func main() {
	url := flag.String("url", defaultURL, "status summary API URL")
	interval := flag.Duration("interval", defaultInterval, "poll interval")
	once := flag.Bool("once", false, "check once and exit")
	notify := flag.Bool("notify", true, "desktop notify when service recovers")
	sound := flag.Bool("sound", true, "loop alarm on recovery until Enter/Ctrl+C")
	inhibit := flag.Bool("inhibit", true, "block system sleep/idle while watching")
	quiet := flag.Bool("quiet", false, "only print status changes")
	component := flag.String("component", "Duolingo", "component name that must be operational")
	testAlert := flag.Bool("test-alert", false, "play recovery sound/notify and exit")
	flag.Parse()

	if *testAlert {
		announceRecovery(*notify, *sound)
		os.Exit(0)
	}

	// Wrap under systemd-inhibit before any work so sleep stays blocked
	// for the whole watch, and we only print status once.
	if !*once && *inhibit {
		if err := ensureSleepInhibited(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not inhibit sleep: %v\n", err)
		} else if os.Getenv(inhibitEnv) == "1" {
			fmt.Println("sleep/idle inhibited while watching")
		}
	}

	client := &http.Client{Timeout: httpTimeout}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lastKey string
	check := func() (healthy bool, err error) {
		sum, err := fetchSummary(ctx, client, *url)
		if err != nil {
			return false, err
		}
		key := statusKey(sum)
		healthy = isHealthy(sum, *component)

		if !*quiet || key != lastKey {
			printStatus(sum, *component, healthy)
			lastKey = key
		}
		return healthy, nil
	}

	healthy, err := check()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if *once {
		if healthy {
			os.Exit(0)
		}
		os.Exit(1)
	}
	if healthy {
		fmt.Println("already operational — nothing to watch")
		os.Exit(0)
	}

	fmt.Printf("watching every %s (Ctrl+C to stop)…\n", interval.String())
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nstopped")
			os.Exit(130)
		case <-ticker.C:
			healthy, err = check()
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s] error: %v\n", time.Now().Format(time.RFC3339), err)
				continue
			}
			if healthy {
				msg := "Duolingo is back online!"
				fmt.Printf("\n✓ %s\n", msg)
				announceRecovery(*notify, *sound)
				os.Exit(0)
			}
		}
	}
}

// ensureSleepInhibited re-execs under systemd-inhibit so idle/sleep stay blocked
// for the lifetime of the watcher process.
func ensureSleepInhibited() error {
	if os.Getenv(inhibitEnv) == "1" {
		return nil
	}
	inhibitPath, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		return fmt.Errorf("systemd-inhibit not found")
	}

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}

	args := append([]string{
		"--what=idle:sleep",
		"--who=duolingo-watcher",
		"--why=Watching Duolingo outage until resolved",
		"--mode=block",
		self,
	}, os.Args[1:]...)

	cmd := exec.Command(inhibitPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), inhibitEnv+"=1")

	err = cmd.Run()
	if err == nil {
		os.Exit(0)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		os.Exit(exitErr.ExitCode())
	}
	return err
}

func fetchSummary(ctx context.Context, client *http.Client, url string) (*summaryResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "duolingo-watcher/1.0")
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, truncate(string(body), 200))
	}

	var sum summaryResponse
	if err := json.Unmarshal(body, &sum); err != nil {
		return nil, fmt.Errorf("decode summary: %w", err)
	}
	return &sum, nil
}

func isHealthy(sum *summaryResponse, wantComponent string) bool {
	if strings.EqualFold(sum.Status.Indicator, "none") && len(sum.Incidents) == 0 {
		compOK := true
		if wantComponent != "" {
			compOK = false
			for _, c := range sum.Components {
				if strings.EqualFold(c.Name, wantComponent) {
					compOK = c.Status == "operational"
					break
				}
			}
		}
		return compOK
	}
	return false
}

func statusKey(sum *summaryResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|", sum.Status.Indicator, sum.Status.Description)
	for _, c := range sum.Components {
		fmt.Fprintf(&b, "%s=%s;", c.Name, c.Status)
	}
	for _, i := range sum.Incidents {
		fmt.Fprintf(&b, "inc:%s=%s;", i.Name, i.Status)
	}
	return b.String()
}

func printStatus(sum *summaryResponse, wantComponent string, healthy bool) {
	ts := time.Now().Format("15:04:05")
	state := "DOWN"
	if healthy {
		state = "UP"
	}

	fmt.Printf("\n[%s] %s — %s (%s)\n", ts, state, sum.Status.Description, sum.Status.Indicator)
	for _, c := range sum.Components {
		mark := " "
		if wantComponent != "" && strings.EqualFold(c.Name, wantComponent) {
			mark = "*"
		}
		fmt.Printf("  %s %-24s %s\n", mark, c.Name, c.Status)
	}
	for _, i := range sum.Incidents {
		fmt.Printf("  ! %-24s %s [%s]\n", truncate(i.Name, 24), i.Status, i.Impact)
		if len(i.Updates) > 0 {
			fmt.Printf("      %s\n", i.Updates[0].Body)
		}
	}
	for _, m := range sum.Scheduled {
		fmt.Printf("  ~ maintenance: %s (%s)\n", m.Name, m.Status)
	}
	if sum.Page.UpdatedAt != "" {
		fmt.Printf("  page updated: %s\n", sum.Page.UpdatedAt)
	}
}

func announceRecovery(notify, sound bool) {
	if notify {
		desktopNotify("Duolingo Watcher", "Duolingo is back online!")
	}
	if !sound {
		return
	}
	if err := playAlarmUntilStopped(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not play sound: %v\n", err)
		fmt.Print("\a\a\a")
	}
}

func desktopNotify(title, body string) {
	if path, err := exec.LookPath("notify-send"); err == nil {
		_ = exec.Command(path, "--urgency=critical", "--app-name=duolingo-watcher", title, body).Run()
		return
	}
	fmt.Print("\a")
}

func playAlarmUntilStopped() error {
	file, err := alarmSoundFile()
	if err != nil {
		return err
	}
	paplay, err := exec.LookPath("paplay")
	if err != nil {
		paplay, err = exec.LookPath("pw-play")
		if err != nil {
			return fmt.Errorf("no audio player found (need paplay or pw-play)")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("🔊 alarm looping — press Enter (or Ctrl+C) to stop")

	enter := make(chan struct{})
	go func() {
		n, err := bufio.NewReader(os.Stdin).ReadBytes('\n')
		// Ignore bare EOF (e.g. backgrounded with no TTY); only stop on real Enter.
		if err != nil && len(n) == 0 {
			return
		}
		close(enter)
	}()

	sinks := audioSinks()
	var mu sync.Mutex
	var current []*exec.Cmd

	killCurrent := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range current {
			if c.Process != nil {
				_ = c.Process.Kill()
			}
		}
		current = nil
	}
	defer killCurrent()

	playOnce := func() {
		mu.Lock()
		current = nil
		var wg sync.WaitGroup
		targets := sinks
		if len(targets) == 0 {
			targets = []string{""}
		}
		for _, sink := range targets {
			wg.Add(1)
			cmd := buildPlayCmd(paplay, file, sink)
			current = append(current, cmd)
			go func(c *exec.Cmd) {
				defer wg.Done()
				_ = c.Run()
			}(cmd)
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-ctx.Done():
			killCurrent()
		case <-enter:
			killCurrent()
		case <-done:
		}
	}

	for {
		select {
		case <-ctx.Done():
			fmt.Println("alarm stopped")
			return nil
		case <-enter:
			fmt.Println("alarm stopped")
			return nil
		default:
		}
		playOnce()
		select {
		case <-ctx.Done():
			fmt.Println("alarm stopped")
			return nil
		case <-enter:
			fmt.Println("alarm stopped")
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func alarmSoundFile() (string, error) {
	for _, f := range []string{soundAlarm, soundComplete} {
		if _, err := os.Stat(f); err == nil {
			return f, nil
		}
	}
	return "", fmt.Errorf("no system sound files found")
}

func buildPlayCmd(player, file, sink string) *exec.Cmd {
	base := filepath.Base(player)
	var cmd *exec.Cmd
	switch base {
	case "paplay":
		args := []string{"--volume=65536"}
		if sink != "" {
			args = append(args, "--device="+sink)
		}
		args = append(args, file)
		cmd = exec.Command(player, args...)
	default: // pw-play
		args := []string{file}
		if sink != "" {
			args = []string{"--target="+sink, file}
		}
		cmd = exec.Command(player, args...)
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd
}

func audioSinks() []string {
	out, err := exec.Command("pactl", "list", "short", "sinks").Output()
	if err != nil {
		return nil
	}
	var sinks []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			sinks = append(sinks, fields[1])
		}
	}
	return sinks
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
