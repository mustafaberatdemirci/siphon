package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// Flags work after the site names too: "siphon doctor bunkr -canary <url>",
// the form the README shows, used to take "-canary" and the URL for site
// names and check the configured canaries instead.
func TestDoctorFlagsAfterSiteNames(t *testing.T) {
	for _, args := range [][]string{
		{"bunkr", "-canary", "https://x.test/a/1", "-v", "mega"},
		{"-canary", "https://x.test/a/1", "bunkr", "-v", "mega"},
		{"bunkr", "mega", "-v", "-canary=https://x.test/a/1"},
	} {
		fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var canaries stringList
		fs.Var(&canaries, "canary", "")
		verbose := fs.Bool("v", false, "")
		names, err := parseInterspersed(fs, args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Join(names, ",") != "bunkr,mega" || strings.Join(canaries, ",") != "https://x.test/a/1" || !*verbose {
			t.Errorf("%v: names %v, canaries %v, verbose %v", args, names, canaries, *verbose)
		}
	}
}
