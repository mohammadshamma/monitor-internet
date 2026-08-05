// Package agent installs and removes the launchd agents that keep the
// collector and dashboard running.
//
// These are user LaunchAgents, not LaunchDaemons, and need no root. On a Mac
// with auto-login enabled and FileVault off, a reboot lands in a live user
// session with nobody present, which is all a LaunchAgent needs. See the README
// for what changes when either of those is not true.
package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/config"
)

const launchctlBin = "/bin/launchctl"

// plistTemplate is intentionally minimal.
//
// Note the absence of ProcessType: the tempting "Background" value lets macOS
// throttle CPU and I/O, which on a fixed-interval prober would distort the very
// timings the tool exists to measure. The default (Standard) is correct.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>WorkingDirectory</key>
	<string>%s</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`

// BinPath is the absolute path launchd should exec.
//
// It resolves the currently running executable rather than assuming a fixed
// location, so the agents work wherever the binary actually lives — ~/go/bin
// from `go install`, ~/.local/bin from `make install`, or a Homebrew prefix.
// Symlinks are resolved because launchd needs a stable target that will not
// change out from under it.
func BinPath() string {
	exe, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		if abs, aerr := filepath.Abs(exe); aerr == nil {
			return abs
		}
		return exe
	}
	// Fall back to the make-install location if the executable cannot be found.
	home, herr := os.UserHomeDir()
	if herr != nil {
		return "/usr/local/bin/mon"
	}
	return filepath.Join(home, ".local", "bin", "mon")
}

func plistPath(label string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func renderPlist(label string, args []string, logName string) string {
	var b strings.Builder
	for _, a := range args {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", a)
	}
	logPath := filepath.Join(config.LogDir(), logName)
	workDir, _ := os.UserHomeDir()
	return fmt.Sprintf(plistTemplate, label, b.String(), workDir, logPath, logPath)
}

func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

// Labels returns the two launchd labels for a configuration.
func Labels(cfg config.Config) []string {
	return []string{cfg.CollectorLabel(), cfg.WebLabel()}
}

// Install writes both plists and loads them.
func Install(cfg config.Config) error {
	if err := config.EnsureDirs(); err != nil {
		return err
	}

	bin := BinPath()
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("cannot locate this binary (looked at %s): %w", bin, err)
	}
	fmt.Printf("agents will exec: %s\n", bin)

	jobs := []struct {
		label   string
		args    []string
		logName string
	}{
		{cfg.CollectorLabel(), []string{bin, "start"}, "collector.log"},
		{cfg.WebLabel(), []string{
			bin, "serve",
			"--host", cfg.Web.Host,
			"--port", fmt.Sprint(cfg.Web.Port),
		}, "web.log"},
	}

	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "LaunchAgents"), 0o755); err != nil {
		return err
	}

	for _, j := range jobs {
		path := plistPath(j.label)
		if err := os.WriteFile(path, []byte(renderPlist(j.label, j.args, j.logName)), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}

		if err := bootstrapJob(j.label, path); err != nil {
			return err
		}
		fmt.Printf("installed and loaded %s\n", j.label)
	}

	fmt.Printf("\nlogs:      %s\n", config.LogDir())
	fmt.Printf("database:  %s\n", config.DBPath())
	fmt.Printf("dashboard: http://localhost:%d/\n", cfg.Web.Port)
	return nil
}

// bootstrapJob loads one job and verifies it actually ended up loaded.
//
// A bootout immediately followed by a bootstrap races inside launchd: the
// bootstrap can report success while the job is torn down a moment later by the
// tail of the bootout. That leaves the plist on disk and nothing running, which
// looks like a healthy install but silently collects no data. So the load is
// confirmed rather than assumed, and retried once if it did not take.
func bootstrapJob(label, path string) error {
	target := domain() + "/" + label

	for attempt := 0; attempt < 2; attempt++ {
		_ = exec.Command(launchctlBin, "bootout", target).Run()
		// Give launchd a moment to finish tearing the old job down.
		time.Sleep(300 * time.Millisecond)

		out, err := exec.Command(launchctlBin, "bootstrap", domain(), path).CombinedOutput()
		if err != nil {
			// Fall back to the legacy API on older launchd behaviour.
			if out2, err2 := exec.Command(launchctlBin, "load", "-w", path).CombinedOutput(); err2 != nil {
				return fmt.Errorf("bootstrap %s: %v (%s); load fallback: %v (%s)",
					label, err, strings.TrimSpace(string(out)),
					err2, strings.TrimSpace(string(out2)))
			}
		}

		time.Sleep(400 * time.Millisecond)
		if exec.Command(launchctlBin, "print", target).Run() == nil {
			return nil
		}
		if attempt == 0 {
			fmt.Printf("warn: %s did not stay loaded; retrying\n", label)
		}
	}
	return fmt.Errorf("%s did not stay loaded after two attempts — check %s",
		label, filepath.Join(config.LogDir(), "collector.log"))
}

// Uninstall unloads both agents and removes their plists.
func Uninstall(cfg config.Config) error {
	for _, label := range Labels(cfg) {
		if out, err := exec.Command(launchctlBin, "bootout", domain()+"/"+label).CombinedOutput(); err != nil {
			// Not loaded is fine; anything else is worth surfacing.
			msg := strings.TrimSpace(string(out))
			if msg != "" && !strings.Contains(msg, "No such process") {
				fmt.Printf("warn: bootout %s: %s\n", label, msg)
			}
		}
		path := plistPath(label)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		fmt.Printf("removed %s\n", label)
	}
	fmt.Println("\nthe database was left in place; delete it manually if you want it gone:")
	fmt.Printf("  %s\n", config.DBPath())
	return nil
}

// Restart kicks both jobs, used after a rebuild.
func Restart(cfg config.Config) error {
	for _, label := range Labels(cfg) {
		target := domain() + "/" + label
		if out, err := exec.Command(launchctlBin, "kickstart", "-k", target).CombinedOutput(); err != nil {
			return fmt.Errorf("kickstart %s: %v (%s)", label, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// Installed reports whether the collector agent is currently loaded.
func Installed(cfg config.Config) bool {
	return exec.Command(launchctlBin, "print", domain()+"/"+cfg.CollectorLabel()).Run() == nil
}
