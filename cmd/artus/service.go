package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// service starts Artus when the machine starts: a systemd unit on Linux, a
// launchd agent on macOS. Elsewhere there is none, and setup says so.
type service struct {
	Kind string // "systemd", "launchd" or ""
	Path string // the unit or agent file
	Exe  string // the Artus program
	User string // the account Artus runs as
	Home string // that account's home directory
	Env  map[string]string
	Root bool // whether this process may write system files itself

	// run executes a command on the user's terminal; tests replace it.
	run func(name string, args ...string) error
}

const (
	systemdUnit  = "/etc/systemd/system/artus.service"
	launchdLabel = "com.pschachhuber.artus"
	// serviceEnv is set for an Artus the service started, which is how setup
	// tells it from one started by hand in a terminal.
	serviceEnv = "ARTUS_SERVICE"
)

// detectService describes how this machine would start Artus at boot.
func detectService() (*service, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	me, err := user.Current()
	if err != nil {
		return nil, err
	}
	s := &service{Exe: exe, User: me.Username, Home: me.HomeDir, Root: os.Geteuid() == 0, run: runAttached}
	// Commands the agent runs should find what the user's own shell finds.
	s.Env = map[string]string{"PATH": os.Getenv("PATH")}
	if dir := os.Getenv("ARTUS_HOME"); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			s.Env["ARTUS_HOME"] = abs
		}
	}
	switch runtime.GOOS {
	case "linux":
		if info, err := os.Stat("/run/systemd/system"); err == nil && info.IsDir() {
			s.Kind, s.Path = "systemd", systemdUnit
		}
	case "darwin":
		s.Kind, s.Path = "launchd", filepath.Join(me.HomeDir, "Library", "LaunchAgents", launchdLabel+".plist")
	}
	return s, nil
}

// runAttached runs a command with this terminal, so that sudo can ask for a
// password and the command's own messages are seen.
func runAttached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// definition returns the unit or agent file.
func (s *service) definition() string {
	if s.Kind == "launchd" {
		return s.plist()
	}
	return s.unit()
}

func (s *service) unit() string {
	var b strings.Builder
	b.WriteString("# Starts Artus when the machine starts. Written by `artus setup`; `artus setup --remove` deletes it.\n")
	b.WriteString("[Unit]\nDescription=Artus personal agent\nDocumentation=https://github.com/pschachhuber/artus\n")
	b.WriteString("After=network-online.target\nWants=network-online.target\n\n[Service]\n")
	fmt.Fprintf(&b, "User=%s\n", s.User)
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", unitPath(s.Home))
	for _, k := range sortedKeys(s.Env) {
		fmt.Fprintf(&b, "Environment=%s\n", unitQuote(k+"="+s.Env[k], false))
	}
	b.WriteString("Environment=\"" + serviceEnv + "=1\"\n")
	fmt.Fprintf(&b, "ExecStart=%s serve --no-open\n", unitQuote(s.Exe, true))
	// An update replaces the program and restarts inside the same process, so
	// systemd only steps in when Artus stops with an error.
	b.WriteString("Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=multi-user.target\n")
	return b.String()
}

// unitQuote writes a word for a systemd unit: quoted, with the characters
// systemd would interpret escaped. Only a command line expands variables.
func unitQuote(s string, command bool) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(s)
	if command {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	return `"` + s + `"`
}

// unitPath writes a path for settings that take one unquoted.
func unitPath(s string) string { return strings.ReplaceAll(s, "%", "%%") }

func (s *service) plist() string {
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<!-- Starts Artus when you log in. Written by `artus setup`; `artus setup -remove` deletes it. -->\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t<string>%s</string>\n", launchdLabel)
	fmt.Fprintf(&b, "\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>%s</string>\n\t\t<string>serve</string>\n\t\t<string>--no-open</string>\n\t</array>\n", esc(s.Exe))
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	for _, k := range sortedKeys(s.Env) {
		fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", esc(k), esc(s.Env[k]))
	}
	fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>1</string>\n", serviceEnv)
	b.WriteString("\t</dict>\n")
	fmt.Fprintf(&b, "\t<key>WorkingDirectory</key>\n\t<string>%s</string>\n", esc(s.Home))
	log := esc(filepath.Join(s.Home, "Library", "Logs", "artus.log"))
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", log, log)
	// Start at login, and again whenever Artus stops with an error.
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys { // a handful of names: no need for the sort package
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// installed reports whether the service file exists.
func (s *service) installed() bool {
	_, err := os.Stat(s.Path)
	return err == nil
}

// sudo runs a command as root: directly when this process is root already.
func (s *service) sudo(name string, args ...string) error {
	if s.Root {
		return s.run(name, args...)
	}
	return s.run("sudo", append([]string{name}, args...)...)
}

// install writes the service file and starts Artus under it, replacing a
// service that already runs, so that it picks up a new program or setting.
func (s *service) install() error {
	switch s.Kind {
	case "systemd":
		if err := s.writeSystemFile(s.Path, s.definition()); err != nil {
			return err
		}
		for _, args := range [][]string{{"daemon-reload"}, {"enable", "artus.service"}, {"restart", "artus.service"}} {
			if err := s.sudo("systemctl", args...); err != nil {
				return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
			}
		}
		return nil
	case "launchd":
		if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(s.Path, []byte(s.definition()), 0o644); err != nil {
			return err
		}
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		s.run("launchctl", "bootout", domain+"/"+launchdLabel) // not loaded yet on a first install
		if err := s.run("launchctl", "bootstrap", domain, s.Path); err != nil {
			return fmt.Errorf("launchctl bootstrap: %w", err)
		}
		return nil
	}
	return errors.New("this system has no service manager Artus knows")
}

// writeSystemFile puts a file where only root may write.
func (s *service) writeSystemFile(path, content string) error {
	if s.Root {
		return os.WriteFile(path, []byte(content), 0o644)
	}
	tmp, err := os.CreateTemp("", "artus-*.service")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := s.run("sudo", "install", "-m", "0644", tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// remove stops the service and deletes its file.
func (s *service) remove() error {
	switch s.Kind {
	case "systemd":
		s.sudo("systemctl", "disable", "--now", "artus.service")
		if err := s.sudo("rm", "-f", s.Path); err != nil {
			return err
		}
		return s.sudo("systemctl", "daemon-reload")
	case "launchd":
		s.run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel))
		return os.Remove(s.Path)
	}
	return nil
}
