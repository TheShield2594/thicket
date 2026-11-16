package main

import (
	"flag"
	"reflect"
	"testing"
)

func TestParseFlagsAnyOrder(t *testing.T) {
	cases := []struct {
		args  []string
		pos   []string
		force bool
		out   string
	}{
		{[]string{"backup.tar.gz", "--force"}, []string{"backup.tar.gz"}, true, ""},
		{[]string{"--force", "backup.tar.gz"}, []string{"backup.tar.gz"}, true, ""},
		{[]string{"juniper", "-o", "copy.md"}, []string{"juniper"}, false, "copy.md"},
		{[]string{"-o", "copy.md", "juniper"}, []string{"juniper"}, false, "copy.md"},
		{[]string{"a", "b"}, []string{"a", "b"}, false, ""},
		// "--" ends flags: a later --force is a positional, not a flag.
		{[]string{"--", "backup.tar.gz", "--force"}, []string{"backup.tar.gz", "--force"}, false, ""},
		{[]string{"backup.tar.gz", "--", "-o"}, []string{"backup.tar.gz", "-o"}, false, ""},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		force := fs.Bool("force", false, "")
		out := fs.String("o", "", "")
		pos, err := parseFlags(fs, c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if !reflect.DeepEqual(pos, c.pos) || *force != c.force || *out != c.out {
			t.Errorf("%v: pos=%v force=%v out=%q", c.args, pos, *force, *out)
		}
	}
}
