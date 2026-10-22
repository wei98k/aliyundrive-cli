package cli

import (
	"strconv"
	"strings"
	"time"

	"aliyundrive-cli/internal/drive"
)

type parsed struct {
	name string
	args []string
	opts map[string]string
}

func (p parsed) boolean(name string) bool { return p.opts[name] == "true" }

func parse(args []string) (parsed, error) {
	if len(args) == 1 && args[0] == "--version" {
		args = []string{"version"}
	}
	p := parsed{opts: map[string]string{}}
	known := map[string]Flag{}
	for _, f := range globals {
		known[f.Name] = f
	}
	for _, c := range commands {
		for _, f := range c.Flags {
			known[f.Name] = f
		}
	}
	var positionals []string
	flags := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" && flags {
			flags = false
			continue
		}
		if flags && (arg == "-h" || arg == "-p" || arg == "-v") {
			switch arg {
			case "-h":
				arg = "--help"
			case "-p":
				arg = "--parents"
			case "-v":
				if len(args) != 1 {
					return p, drive.Invalid("-v must be used alone")
				}
				arg = "version"
			}
		}
		if !flags || !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		if !strings.HasPrefix(arg, "--") {
			return p, drive.Invalid("unknown flag %q", arg)
		}
		key, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		f, ok := known[key]
		if !ok {
			return p, drive.Invalid("unknown flag --%s", key)
		}
		if _, duplicate := p.opts[key]; duplicate {
			return p, drive.Invalid("flag --%s was provided more than once", key)
		}
		if f.Type == "boolean" {
			if !hasValue {
				value = "true"
			}
			v, err := strconv.ParseBool(value)
			if err != nil {
				return p, drive.Invalid("--%s expects true or false", key)
			}
			value = strconv.FormatBool(v)
		} else if !hasValue {
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "--") {
				return p, drive.Invalid("--%s requires a value", key)
			}
			i++
			value = args[i]
		}
		if f.Type != "boolean" && value == "" {
			return p, drive.Invalid("--%s cannot be empty", key)
		}
		p.opts[key] = value
	}
	if len(positionals) == 0 {
		p.name = "help"
	} else {
		p.name, p.args = positionals[0], positionals[1:]
		if p.name == "auth" {
			if len(p.args) == 0 && p.boolean("help") {
				p.name = "help"
				p.args = []string{"auth"}
			} else if len(p.args) == 0 {
				return p, drive.Invalid("auth requires a subcommand: login, poll, status, import, logout")
			} else {
				p.name += " " + p.args[0]
				p.args = p.args[1:]
			}
		}
	}
	c := findCommand(p.name)
	if c == nil {
		return p, drive.Invalid("unknown command %q", p.name)
	}
	allowed := map[string]bool{}
	for _, f := range globals {
		allowed[f.Name] = true
	}
	for _, f := range c.Flags {
		allowed[f.Name] = true
	}
	for key := range p.opts {
		if !allowed[key] {
			return p, drive.Invalid("--%s is not supported by %s", key, p.name)
		}
	}
	if p.boolean("help") {
		return p, nil
	}
	if c.Arguments >= 0 && len(p.args) != c.Arguments {
		return p, drive.Invalid("usage: aliyundrive %s", c.Usage)
	}
	if p.name == "ls" && len(p.args) > 1 {
		return p, drive.Invalid("ls accepts at most one remote folder")
	}
	if v := p.opts["limit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return p, drive.Invalid("--limit must be between 1 and 100")
		}
	}
	if _, err := p.timeout(); err != nil {
		return p, err
	}
	return p, nil
}

func (p parsed) timeout() (time.Duration, error) {
	v := p.opts["timeout"]
	if v == "" {
		v = "5m"
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, drive.Invalid("--timeout must be a positive duration, e.g. 30s or 5m")
	}
	return d, nil
}

func (p parsed) limit() int {
	if p.opts["limit"] == "" {
		return 100
	}
	n, _ := strconv.Atoi(p.opts["limit"])
	return n
}
