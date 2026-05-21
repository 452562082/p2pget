package main

import (
	"flag"
	"reflect"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantPos []string
		wantN   int
	}{
		{"flags before positional", []string{"-n", "5", "ubuntu"}, []string{"ubuntu"}, 5},
		{"flags after positional", []string{"ubuntu", "-n", "5"}, []string{"ubuntu"}, 5},
		{"interleaved", []string{"ubuntu", "-n", "3", "server"}, []string{"ubuntu", "server"}, 3},
		{"no flags", []string{"a", "b"}, []string{"a", "b"}, 20},
		{"only flags", []string{"-n", "7"}, nil, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			n := fs.Int("n", 20, "")
			pos := parseArgs(fs, c.args)
			if !reflect.DeepEqual(pos, c.wantPos) {
				t.Errorf("positionals=%v want %v", pos, c.wantPos)
			}
			if *n != c.wantN {
				t.Errorf("-n=%d want %d", *n, c.wantN)
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	if got := envOr("P2PGET_UNSET_VAR_XYZ", "fallback"); got != "fallback" {
		t.Errorf("unset var: got %q want fallback", got)
	}
	t.Setenv("P2PGET_TEST_VAR", "value")
	if got := envOr("P2PGET_TEST_VAR", "fallback"); got != "value" {
		t.Errorf("set var: got %q want value", got)
	}
}
