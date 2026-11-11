package browser

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// commandSpec describes one CLI command.
type commandSpec struct {
	name    string
	aliases []string
	group   string
	usage   string // arguments and flags, as shown in help
	help    string
	minArgs int
	maxArgs int // -1 for unlimited
	flags   []string
	// needsBrowser commands run against a session, starting its browser when
	// it is not running.
	needsBrowser bool
	run          func(*command) error
}

// boolFlags are flags that take no value; every other flag does.
var boolFlags = map[string]bool{
	"headed": true, "headless": true, "isolated": true, "regex": true,
	"submit": true, "full": true, "load": true, "force": true,
}

// globalFlags are accepted by every command.
var globalFlags = map[string]bool{
	"session": true, "headed": true, "headless": true, "isolated": true,
	"profile": true, "timeout": true,
}

// invocation is a parsed command line.
type invocation struct {
	cmd     string
	spec    *commandSpec
	args    []string
	flags   map[string]string
	session string
}

// parseArgs splits argv into a command, its positional arguments and flags.
// Flags may appear anywhere; "--" ends flag parsing so that text starting
// with dashes can be passed.
func parseArgs(argv []string) (*invocation, error) {
	inv := &invocation{flags: map[string]string{}, session: "default"}
	var positional []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			positional = append(positional, argv[i+1:]...)
			i = len(argv)
		case arg == "-s" || strings.HasPrefix(arg, "-s="):
			arg = "--session" + strings.TrimPrefix(arg, "-s")
			fallthrough
		case strings.HasPrefix(arg, "--") && len(arg) > 2:
			name, value, hasValue := strings.Cut(arg[2:], "=")
			switch {
			case boolFlags[name]:
				if !hasValue {
					value = "true"
				}
			case !hasValue:
				if i+1 >= len(argv) {
					return nil, fmt.Errorf("flag --%s needs a value", name)
				}
				i++
				value = argv[i]
			}
			inv.flags[name] = value
		default:
			positional = append(positional, arg)
		}
	}
	if name, ok := inv.flags["session"]; ok {
		if name == "" || strings.ContainsAny(name, `/\. `) {
			return nil, fmt.Errorf("invalid session name %q", name)
		}
		inv.session = name
	}
	if len(positional) == 0 {
		positional = []string{"help"}
	}
	inv.cmd, inv.args = positional[0], positional[1:]
	inv.spec = findCommand(inv.cmd)
	if inv.spec == nil {
		return nil, fmt.Errorf("unknown command %q; run `artus browser help` for the command list", inv.cmd)
	}
	inv.cmd = inv.spec.name
	if len(inv.args) < inv.spec.minArgs || (inv.spec.maxArgs >= 0 && len(inv.args) > inv.spec.maxArgs) {
		return nil, fmt.Errorf("usage: artus browser %s %s", inv.spec.name, inv.spec.usage)
	}
	for name := range inv.flags {
		if !globalFlags[name] && !slices.Contains(inv.spec.flags, name) {
			return nil, fmt.Errorf("unknown flag --%s for %s; usage: artus browser %s %s", name, inv.spec.name, inv.spec.name, inv.spec.usage)
		}
	}
	if inv.has("headed") && inv.has("headless") {
		return nil, fmt.Errorf("--headed and --headless are mutually exclusive")
	}
	return inv, nil
}

func findCommand(name string) *commandSpec {
	for i := range commands {
		if commands[i].name == name || slices.Contains(commands[i].aliases, name) {
			return &commands[i]
		}
	}
	return nil
}

// has reports whether a flag was given (and, for boolean flags, not false).
func (inv *invocation) has(flag string) bool {
	v, ok := inv.flags[flag]
	return ok && v != "false"
}

// arg returns the i-th positional argument, or "" when absent.
func (inv *invocation) arg(i int) string {
	if i < len(inv.args) {
		return inv.args[i]
	}
	return ""
}

// intFlag parses an integer flag, returning def when it is absent.
func (inv *invocation) intFlag(flag string, def int) (int, error) {
	v, ok := inv.flags[flag]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--%s must be a non-negative integer, not %q", flag, v)
	}
	return n, nil
}

// timeout returns the --timeout override in milliseconds, or def.
func (inv *invocation) timeout(def time.Duration) (time.Duration, error) {
	ms, err := inv.intFlag("timeout", int(def/time.Millisecond))
	return time.Duration(ms) * time.Millisecond, err
}

// sortedKeys returns the keys of m in sorted order.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
