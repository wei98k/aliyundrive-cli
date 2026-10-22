package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"aliyundrive-cli/internal/drive"
)

func decodeEnvelope(t *testing.T, output *bytes.Buffer) envelope {
	t.Helper()
	d := json.NewDecoder(output)
	var got envelope
	if err := d.Decode(&got); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout contains more than one JSON envelope: %v", err)
	}
	if got.SchemaVersion != "1" {
		t.Fatalf("schema_version = %q", got.SchemaVersion)
	}
	return got
}

func TestOfflineCommandsAndGuardsDoNotCreateClient(t *testing.T) {
	r := runner{newClient: func(string, time.Duration) (*drive.Client, error) {
		t.Fatal("unexpected client creation")
		return nil, nil
	}}
	tests := []struct {
		args      []string
		exit      int
		errorCode string
	}{
		{nil, 0, ""}, {[]string{"schema"}, 0, ""}, {[]string{"version"}, 0, ""},
		{[]string{"help", "auth"}, 0, ""}, {[]string{"help", "upload"}, 0, ""},
		{[]string{"download", "--help", "--pretty"}, 0, ""},
		{[]string{"trash", "id:abc"}, 5, "CONFIRMATION_REQUIRED"},
		{[]string{"trash", "id:abc", "--yes=false"}, 5, "CONFIRMATION_REQUIRED"},
		{[]string{"auth", "import"}, 2, "INVALID_ARGUMENT"},
		{[]string{"auth", "import", "--stdin", "--token-file", "file"}, 2, "INVALID_ARGUMENT"},
		{[]string{"stat"}, 2, "INVALID_ARGUMENT"}, {[]string{"help", "bogus"}, 2, "INVALID_ARGUMENT"},
	}
	for _, tt := range tests {
		t.Run(joinTestArgs(tt.args), func(t *testing.T) {
			var out, errOut bytes.Buffer
			exit := r.run(context.Background(), tt.args, strings.NewReader(""), &out, &errOut)
			if exit != tt.exit {
				t.Fatalf("exit = %d, output = %s", exit, out.String())
			}
			got := decodeEnvelope(t, &out)
			if got.OK != (tt.exit == 0) {
				t.Fatalf("ok = %v", got.OK)
			}
			if tt.errorCode != "" && (got.Error == nil || got.Error.Code != tt.errorCode) {
				t.Fatalf("error = %#v", got.Error)
			}
			if errOut.Len() != 0 {
				t.Fatalf("stderr = %q", errOut.String())
			}
		})
	}
}

func TestClientErrorsAreJSON(t *testing.T) {
	r := runner{newClient: func(string, time.Duration) (*drive.Client, error) {
		return nil, drive.E("IO_ERROR", "Cannot read configuration.", "", 7)
	}}
	var out, errOut bytes.Buffer
	if exit := r.run(context.Background(), []string{"ls"}, strings.NewReader(""), &out, &errOut); exit != 7 {
		t.Fatalf("exit = %d", exit)
	}
	got := decodeEnvelope(t, &out)
	if got.Error.Code != "IO_ERROR" || got.OK {
		t.Fatalf("envelope = %#v", got)
	}
}

func TestNormalizeError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
		exit int
	}{
		{context.Canceled, "CANCELED", 8}, {context.DeadlineExceeded, "TIMEOUT", 8},
		{drive.E("NOT_FOUND", "missing", "", 4), "NOT_FOUND", 4},
		{errors.New("secret upstream body"), "INTERNAL_ERROR", 6},
	} {
		got := normalizeError(tc.err)
		if got.Code != tc.code || got.ExitCode != tc.exit {
			t.Fatalf("error = %#v", got)
		}
		if strings.Contains(got.Message, "secret") {
			t.Fatal("leaked raw error")
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestOutputFailure(t *testing.T) {
	var errOut bytes.Buffer
	exit := Run(context.Background(), []string{"version"}, strings.NewReader(""), failingWriter{}, &errOut)
	if exit != 7 || errOut.String() != "cannot write JSON output\n" {
		t.Fatalf("exit %d, stderr %q", exit, errOut.String())
	}
}
