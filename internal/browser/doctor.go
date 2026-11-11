package browser

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// aptPackages maps shared libraries Chromium needs to the Debian/Ubuntu
// packages providing them.
var aptPackages = map[string]string{
	"libnss3.so": "libnss3", "libnssutil3.so": "libnss3", "libsmime3.so": "libnss3", "libnspr4.so": "libnspr4",
	"libatk-1.0.so.0": "libatk1.0-0", "libatk-bridge-2.0.so.0": "libatk-bridge2.0-0", "libatspi.so.0": "libatspi2.0-0",
	"libcups.so.2": "libcups2", "libdrm.so.2": "libdrm2", "libgbm.so.1": "libgbm1", "libxkbcommon.so.0": "libxkbcommon0",
	"libXcomposite.so.1": "libxcomposite1", "libXdamage.so.1": "libxdamage1", "libXfixes.so.3": "libxfixes3",
	"libXrandr.so.2": "libxrandr2", "libXext.so.6": "libxext6", "libX11.so.6": "libx11-6", "libxcb.so.1": "libxcb1",
	"libpango-1.0.so.0": "libpango-1.0-0", "libcairo.so.2": "libcairo2", "libasound.so.2": "libasound2",
	"libdbus-1.so.3": "libdbus-1-3", "libglib-2.0.so.0": "libglib2.0-0", "libgobject-2.0.so.0": "libglib2.0-0",
	"libgio-2.0.so.0": "libglib2.0-0", "libexpat.so.1": "libexpat1",
}

// missingLibraries lists the shared libraries exe needs but the system
// lacks, according to ldd.
func missingLibraries(exe string) ([]string, error) {
	binary := elfBinary(exe)
	if binary == "" {
		return nil, errors.New("the browser is started through a wrapper script whose binary was not found")
	}
	out, err := exec.Command("ldd", binary).Output()
	if err != nil && len(out) == 0 {
		return nil, err
	}
	var missing []string
	for _, line := range strings.Split(string(out), "\n") {
		if name, _, ok := strings.Cut(line, "=> not found"); ok {
			missing = append(missing, strings.TrimSpace(name))
		}
	}
	return missing, nil
}

// elfBinary returns the ELF executable behind a browser path. Distribution
// packages install a wrapper script; the binary is then a sibling of the
// script or named in it.
func elfBinary(exe string) string {
	isELF := func(path string) bool {
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		defer f.Close()
		magic := make([]byte, 4)
		_, err = io.ReadFull(f, magic)
		return err == nil && string(magic) == "\x7fELF"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if isELF(exe) {
		return exe
	}
	var candidates []string
	for _, name := range []string{"chrome", "msedge", "brave", "chromium"} {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	if script, err := os.ReadFile(exe); err == nil && len(script) < 1<<20 {
		candidates = append(candidates, regexp.MustCompile(`/[^\s"'$]+`).FindAllString(string(script), -1)...)
	}
	for _, c := range candidates {
		if resolved, err := filepath.EvalSymlinks(c); err == nil && isELF(resolved) {
			return resolved
		}
	}
	return ""
}

// installHint suggests how to install missing libraries with the system's
// package manager.
func installHint(missing []string) string {
	if _, err := exec.LookPath("apt-get"); err == nil {
		var pkgs []string
		for _, lib := range missing {
			if pkg, ok := aptPackages[lib]; ok && !slices.Contains(pkgs, pkg) {
				pkgs = append(pkgs, pkg)
			}
		}
		if len(pkgs) > 0 {
			return "Try: sudo apt-get install -y " + strings.Join(pkgs, " ") + "\n(package names vary between releases, e.g. libasound2t64 on Ubuntu 24.04 and later)"
		}
		return "Find the packages with: apt-file search <library>"
	}
	for manager, query := range map[string]string{
		"dnf": "dnf provides '*/<library>'", "yum": "yum provides '*/<library>'",
		"zypper": "zypper search --provides <library>", "pacman": "pacman -F <library>", "apk": "apk search <library>",
	} {
		if _, err := exec.LookPath(manager); err == nil {
			return "Find the packages providing them with: " + query
		}
	}
	return "Install the packages providing these libraries with your distribution's package manager."
}

func cmdDoctor(c *command) error {
	dir, cfg := c.m.opts.Dir, c.cfg
	c.out.say("Directory: %s", dir)
	c.out.say("Configuration: %s", configPath(dir))
	if cfg.CDPEndpoint != "" {
		c.out.say("cdpEndpoint is set: sessions attach to the browser at %s instead of starting one.", cfg.CDPEndpoint)
	}
	found, source, err := c.m.resolveBrowser(c.ctx, cfg, false)
	if err != nil {
		c.out.say("Browser: none (%v). `open` downloads Chrome for Testing automatically when needed; `install` does it now.", err)
	} else {
		c.out.say("Browser: %s (%s, %s)", found.path, found.kind, source)
		c.out.say("Version: %s", browserVersion(c.ctx, found.path))
	}
	switch {
	case cfg.Headless == headlessOn:
		c.out.say("Mode: headless (configured)")
	case cfg.Headless == headlessOff:
		c.out.say("Mode: headed (configured)")
	case displayAvailable():
		c.out.say("Mode: headed (a display is available)")
	default:
		c.out.say("Mode: headless (no DISPLAY or WAYLAND_DISPLAY)")
	}
	switch {
	case runningAsRoot():
		c.out.say("Sandbox: disabled, because Chromium does not run sandboxed as root")
	case cfg.NoSandbox:
		c.out.say("Sandbox: disabled by noSandbox in the configuration")
	default:
		c.out.say("Sandbox: enabled")
	}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		if os.Getenv(name) != "" {
			c.out.say("Downloads use the proxy from %s.", name)
			break
		}
	}
	if err != nil || runtime.GOOS != "linux" {
		return nil
	}
	missing, err := missingLibraries(found.path)
	switch {
	case err != nil:
		c.out.say("System libraries: could not be checked (%v)", err)
	case len(missing) == 0:
		c.out.say("System libraries: all present")
	default:
		c.out.say("System libraries: MISSING %s", strings.Join(missing, ", "))
		c.out.say("%s", installHint(missing))
	}
	return nil
}

func cmdInstall(c *command) error {
	rec, fresh, err := c.m.installChrome(c.ctx, c.inv.has("force"))
	if err != nil {
		return err
	}
	exe := rec.executable(c.m.opts.Dir)
	if fresh {
		c.out.say("Installed Chrome for Testing %s at %s", rec.Version, exe)
	} else {
		c.out.say("Chrome for Testing %s is already installed at %s (it is the current stable build; --force downloads it again)", rec.Version, exe)
	}
	if c.cfg.ExecutablePath != "" || c.cfg.CDPEndpoint != "" {
		c.out.say("Note: the configuration sets executablePath or cdpEndpoint, which take precedence over this download.")
	} else if found, _, err := c.m.resolveBrowser(c.ctx, c.cfg, false); err == nil && found.path != exe {
		c.out.say("Note: sessions use %s, which is installed on this machine; set \"browser\": \"chrome-for-testing\" in %s to use the download.", found.path, configPath(c.m.opts.Dir))
	}
	return nil
}
