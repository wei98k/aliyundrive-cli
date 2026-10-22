package cli

// Version can be set with -ldflags '-X aliyundrive-cli/internal/cli.Version=...'.
var Version = "0.1.0"

type Flag struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description"`
}

type Command struct {
	Name        string   `json:"name"`
	Usage       string   `json:"usage"`
	Description string   `json:"description"`
	Mutates     bool     `json:"mutates"`
	Arguments   int      `json:"argument_count"`
	Flags       []Flag   `json:"flags"`
	Examples    []string `json:"examples"`
}

var globals = []Flag{
	{"config-dir", "string", "", "Credential directory; default: $XDG_CONFIG_HOME/aliyundrive or ~/.config/aliyundrive."},
	{"drive-id", "string", "", "Override the account's default drive ID."},
	{"timeout", "duration", "5m", "Whole-command deadline, including login waits and transfers."},
	{"pretty", "boolean", "false", "Indent JSON output."},
	{"help", "boolean", "false", "Return command help without authentication or side effects."},
}

var dryFlag = Flag{"dry-run", "boolean", "false", "Return a plan without file mutations or transfer writes; existing credentials may be refreshed and saved."}
var pageFlags = []Flag{
	{"limit", "integer", "100", "Page size (1..100)."},
	{"marker", "string", "", "Opaque continuation marker from the previous page."},
	{"all", "boolean", "false", "Fetch every page; next_marker is empty on completion."},
}

var commands = []Command{
	{"help", "help [COMMAND]", "Return help as JSON.", false, -1, nil, []string{"aliyundrive help upload"}},
	{"schema", "schema", "Return the complete machine-readable CLI contract. Does not access the account.", false, 0, nil, []string{"aliyundrive schema"}},
	{"version", "version", "Return CLI and output schema versions.", false, 0, nil, []string{"aliyundrive version"}},
	{"auth login", "auth login", "Create a QR login session and local PNG; scan it, then run auth poll. Never blocks for input.", true, 0, nil, []string{"aliyundrive auth login"}},
	{"auth poll", "auth poll [--wait]", "Check the current QR session; --wait polls until a terminal state or the deadline.", true, 0, []Flag{{"wait", "boolean", "false", "Wait for authentication, cancellation, or expiration."}}, []string{"aliyundrive auth poll --wait --timeout 3m"}},
	{"auth status", "auth status [--check]", "Inspect local credential presence; --check validates the account online (and may refresh tokens).", false, 0, []Flag{{"check", "boolean", "false", "Validate credentials online and return account metadata."}}, []string{"aliyundrive auth status --check"}},
	{"auth import", "auth import (--token-file FILE | --stdin)", "Import a refresh token or legacy SDK token JSON. Tokens are never accepted as command arguments.", true, 0, []Flag{{"token-file", "string", "", "Read a refresh token or token JSON from this file."}, {"stdin", "boolean", "false", "Read token data from stdin; never prompts."}}, []string{"aliyundrive auth import --token-file ~/.go-aliyundrive-sdk/token.json"}},
	{"auth logout", "auth logout", "Remove this CLI's local credentials and QR login artifacts; does not revoke other sessions.", true, 0, nil, []string{"aliyundrive auth logout"}},
	{"ls", "ls [REMOTE] [--limit N] [--marker MARKER] [--all]", "List a folder (default /). Returns stable file IDs and a continuation marker.", false, -1, pageFlags, []string{"aliyundrive ls / --all", "aliyundrive ls id:FOLDER_ID --limit 50"}},
	{"stat", "stat REMOTE", "Resolve an absolute path or file ID and return metadata.", false, 1, nil, []string{"aliyundrive stat '/Documents/report.pdf'"}},
	{"search", "search NAME [--limit N] [--marker MARKER] [--all]", "Search names across the selected drive. NAME is a value, not an executable query.", false, 1, pageFlags, []string{"aliyundrive search 'report' --all"}},
	{"mkdir", "mkdir REMOTE [--parents] [--dry-run]", "Create a folder; refuse name conflicts. --parents creates missing ancestors and accepts existing folders.", true, 1, []Flag{{"parents", "boolean", "false", "Create missing ancestors (-p)."}, dryFlag}, []string{"aliyundrive mkdir /Backups/2026 --parents --dry-run"}},
	{"rename", "rename REMOTE NEW_NAME [--dry-run]", "Rename one file or folder; refuse name conflicts.", true, 2, []Flag{dryFlag}, []string{"aliyundrive rename id:FILE_ID 'new name.pdf' --dry-run"}},
	{"mv", "mv REMOTE DESTINATION_FOLDER [--dry-run]", "Move one file or folder into an existing folder; no overwrite or implicit rename.", true, 2, []Flag{dryFlag}, []string{"aliyundrive mv id:FILE_ID /Archive --dry-run"}},
	{"trash", "trash REMOTE (--yes | --dry-run)", "Move an item to the recycle bin. Never permanently deletes. Requires explicit --yes.", true, 1, []Flag{{"yes", "boolean", "false", "Explicitly confirm moving this item to the recycle bin."}, dryFlag}, []string{"aliyundrive trash id:FILE_ID --dry-run", "aliyundrive trash id:FILE_ID --yes"}},
	{"upload", "upload LOCAL_FILE REMOTE_FOLDER [--name NAME] [--dry-run]", "Synchronously upload one regular file using streaming multipart transfer. Refuse conflicts; directories are not supported.", true, 2, []Flag{{"name", "string", "", "Use this remote filename instead of the local basename."}, dryFlag}, []string{"aliyundrive upload ./report.pdf /Documents --dry-run"}},
	{"download", "download REMOTE_FILE LOCAL_PATH [--overwrite] [--dry-run]", "Synchronously download one file to an explicit path. Publish only a complete download; refuse existing files by default.", true, 2, []Flag{{"overwrite", "boolean", "false", "Explicitly allow replacement of the local destination file."}, dryFlag}, []string{"aliyundrive download id:FILE_ID ./report.pdf --dry-run"}},
}

func schema() any {
	return map[string]any{
		"name": "aliyundrive", "version": Version, "schema_version": "1",
		"output":            map[string]any{"format": "json", "stdout": "Exactly one envelope per invocation, including errors and help.", "envelope": "{schema_version,ok,command,data?,error?}", "error_fields": []string{"code", "message", "hint?", "retryable", "details?"}},
		"remote_references": []string{"/absolute/path", "id:FILE_ID", "root"},
		"global_flags":      globals, "commands": commands,
		"exit_codes":  map[string]string{"0": "success (check auth poll status for pending/expired/canceled)", "2": "invalid usage", "3": "authentication required or rejected", "4": "not found", "5": "conflict or confirmation required", "6": "upstream or network error", "7": "local I/O or configuration error", "8": "canceled or command deadline exceeded"},
		"environment": map[string]string{"ALIYUNDRIVE_REFRESH_TOKEN": "Optional refresh token for this invocation; persisted after rotation.", "XDG_CONFIG_HOME": "Overrides the default config parent directory."},
		"api_family":  "Aliyun Drive web API; not registered-app Open Platform OAuth. Live account compatibility must be verified separately.",
	}
}

func findCommand(name string) *Command {
	for i := range commands {
		if commands[i].Name == name {
			return &commands[i]
		}
	}
	return nil
}
