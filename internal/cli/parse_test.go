package cli

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		args []string
		name string
		pos  []string
		opts map[string]string
	}{
		{nil, "help", nil, nil},
		{[]string{"--version"}, "version", nil, nil},
		{[]string{"-v"}, "version", nil, nil},
		{[]string{"--pretty", "ls", "/a b", "--limit=10", "--all=false"}, "ls", []string{"/a b"}, map[string]string{"pretty": "true", "limit": "10", "all": "false"}},
		{[]string{"mkdir", "-p", "/a/b", "--dry-run"}, "mkdir", []string{"/a/b"}, map[string]string{"parents": "true", "dry-run": "true"}},
		{[]string{"rename", "id:abc", "--", "--name.pdf"}, "rename", []string{"id:abc", "--name.pdf"}, nil},
		{[]string{"auth", "poll", "--wait", "--timeout", "1m"}, "auth poll", nil, map[string]string{"wait": "true", "timeout": "1m"}},
		{[]string{"auth", "--help"}, "help", []string{"auth"}, map[string]string{"help": "true"}},
		{[]string{"download", "--help"}, "download", nil, map[string]string{"help": "true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+joinTestArgs(tt.args), func(t *testing.T) {
			p, err := parse(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if p.name != tt.name {
				t.Fatalf("command = %q, want %q", p.name, tt.name)
			}
			if len(p.args) != len(tt.pos) || (len(tt.pos) != 0 && !reflect.DeepEqual(p.args, tt.pos)) {
				t.Fatalf("args = %#v, want %#v", p.args, tt.pos)
			}
			for k, v := range tt.opts {
				if p.opts[k] != v {
					t.Errorf("%s = %q, want %q", k, p.opts[k], v)
				}
			}
		})
	}
}

func joinTestArgs(args []string) string {
	var s string
	for _, arg := range args {
		s += arg + " "
	}
	return s
}

func TestParseRejectsInvalidUsage(t *testing.T) {
	tests := [][]string{
		{"unknown"}, {"auth"}, {"auth", "unknown"}, {"stat"}, {"stat", "/", "extra"}, {"ls", "/", "/a"},
		{"ls", "--limit", "0"}, {"ls", "--limit", "101"}, {"ls", "--limit", "bad"}, {"ls", "--limit"},
		{"ls", "--limit=1", "--limit=2"}, {"ls", "--all=maybe"}, {"ls", "--unknown"}, {"ls", "-q"},
		{"stat", "/", "--dry-run"}, {"upload", "a", "/", "--yes"}, {"version", "extra"},
		{"ls", "--timeout", "0s"}, {"ls", "--timeout", "bad"}, {"ls", "--timeout", "-1s"},
		{"ls", "--config-dir="}, {"mkdir", "/a", "--parents", "--parents"},
		{"help", "--dry-run"}, {"help", "--timeout", "bad"},
	}
	for _, args := range tests {
		t.Run(joinTestArgs(args), func(t *testing.T) {
			if _, err := parse(args); err == nil {
				t.Fatal("expected invalid usage")
			}
		})
	}
}
