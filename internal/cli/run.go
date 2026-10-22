package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"aliyundrive-cli/internal/drive"
)

type envelope struct {
	SchemaVersion string       `json:"schema_version"`
	OK            bool         `json:"ok"`
	Command       string       `json:"command"`
	Data          any          `json:"data,omitempty"`
	Error         *drive.Error `json:"error,omitempty"`
}

type runner struct {
	newClient     func(string, time.Duration) (*drive.Client, error)
	newAuthClient func(string, time.Duration) (*drive.Client, error)
}

func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	return runner{newClient: drive.NewClient, newAuthClient: drive.NewAuthClient}.run(ctx, args, in, out, errOut)
}

func (r runner) run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	p, err := parse(args)
	var data any
	if err == nil {
		data, err = r.execute(ctx, p, in)
	}
	result := envelope{SchemaVersion: "1", OK: err == nil, Command: p.name, Data: data}
	if result.Command == "" {
		result.Command = "unknown"
	}
	code := 0
	if err != nil {
		result.Data = nil
		result.Error = normalizeError(err)
		// Expose known partial effects without treating the operation as successful.
		var details any
		switch partial := data.(type) {
		case drive.Mutation:
			if len(partial.Created) != 0 {
				details = map[string]any{"created": partial.Created}
			}
		case drive.Transfer:
			if partial.Operation == "upload" && partial.File.FileID != "" {
				details = map[string]any{"remote_file": partial.File}
			}
		}
		if details != nil {
			copy := *result.Error
			copy.Details = details
			result.Error = &copy
		}
		code = result.Error.ExitCode
		if code == 0 {
			code = 6
		}
	}
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	if p.boolean("pretty") {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintln(errOut, "cannot write JSON output")
		return 7
	}
	return code
}

func normalizeError(err error) *drive.Error {
	if errors.Is(err, context.Canceled) {
		return drive.E("CANCELED", "Command canceled.", "", 8)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return drive.E("TIMEOUT", "Command deadline exceeded.", "Increase --timeout if appropriate.", 8)
	}
	var typed *drive.Error
	if errors.As(err, &typed) {
		return typed
	}
	return drive.E("INTERNAL_ERROR", "Command failed unexpectedly.", "Check local configuration and retry; report reproducible failures.", 6)
}

func (r runner) execute(ctx context.Context, p parsed, in io.Reader) (any, error) {
	if p.name == "help" {
		return help(p.args)
	}
	if p.boolean("help") {
		return help(strings.Fields(p.name))
	}
	switch p.name {
	case "schema":
		return schema(), nil
	case "version":
		return map[string]string{"version": Version, "schema_version": "1"}, nil
	}
	// Validate operation-specific requirements before touching credentials or the network.
	if p.name == "trash" && !p.boolean("yes") && !p.boolean("dry-run") {
		return nil, drive.E("CONFIRMATION_REQUIRED", "trash requires --yes or --dry-run.", "Preview the item with --dry-run, then explicitly confirm with --yes.", 5)
	}
	if p.name == "auth import" && ((p.opts["token-file"] != "") == p.boolean("stdin")) {
		return nil, drive.Invalid("auth import requires exactly one of --token-file FILE or --stdin")
	}
	timeout, err := p.timeout()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	factory := r.newClient
	if r.newAuthClient != nil && (p.name == "auth login" || p.name == "auth poll" || p.name == "auth import" || p.name == "auth logout") {
		factory = r.newAuthClient
	}
	c, err := factory(p.opts["config-dir"], timeout)
	if err != nil {
		return nil, err
	}
	c.DriveID = p.opts["drive-id"]
	dry := p.boolean("dry-run")
	switch p.name {
	case "auth login":
		return c.LoginStart(ctx)
	case "auth poll":
		return c.LoginPoll(ctx, p.boolean("wait"))
	case "auth status":
		return c.Status(ctx, p.boolean("check"))
	case "auth logout":
		if err := c.Logout(); err != nil {
			return nil, err
		}
		return map[string]bool{"logged_out": true}, nil
	case "auth import":
		if p.boolean("stdin") {
			return c.ImportToken(ctx, in)
		}
		file, err := os.Open(p.opts["token-file"])
		if err != nil {
			return nil, drive.E("IO_ERROR", "Cannot open token file.", "Check --token-file and file permissions.", 7)
		}
		defer file.Close()
		return c.ImportToken(ctx, file)
	case "ls":
		ref := "/"
		if len(p.args) != 0 {
			ref = p.args[0]
		}
		return c.List(ctx, ref, p.limit(), p.opts["marker"], p.boolean("all"))
	case "stat":
		return c.Resolve(ctx, p.args[0])
	case "search":
		return c.Search(ctx, p.args[0], p.limit(), p.opts["marker"], p.boolean("all"))
	case "mkdir":
		return c.Mkdir(ctx, p.args[0], p.boolean("parents"), dry)
	case "rename":
		return c.Rename(ctx, p.args[0], p.args[1], dry)
	case "mv":
		return c.Move(ctx, p.args[0], p.args[1], dry)
	case "trash":
		return c.Trash(ctx, p.args[0], dry)
	case "upload":
		return c.Upload(ctx, p.args[0], p.args[1], p.opts["name"], dry)
	case "download":
		return c.Download(ctx, p.args[0], p.args[1], p.boolean("overwrite"), dry)
	}
	return nil, drive.Invalid("unknown command %q", p.name)
}

func help(args []string) (any, error) {
	if len(args) == 0 {
		return schema(), nil
	}
	name := strings.Join(args, " ")
	if name == "auth" {
		var auth []Command
		for _, c := range commands {
			if strings.HasPrefix(c.Name, "auth ") {
				auth = append(auth, c)
			}
		}
		return map[string]any{"commands": auth, "global_flags": globals}, nil
	}
	c := findCommand(name)
	if c == nil {
		return nil, drive.Invalid("unknown help topic %q", name)
	}
	return map[string]any{"command": c, "global_flags": globals}, nil
}
