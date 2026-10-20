package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeRegistration replaces the browser registration for a test: reg is what
// is registered, and installs are recorded.
func fakeRegistration(t *testing.T, reg string) *[]string {
	t.Helper()
	var installed []string
	oldReg, oldInstall := registeredHost, installHost
	registeredHost = func() string { return reg }
	installHost = func(host string) ([]string, error) {
		installed = append(installed, host)
		return []string{"Chrome"}, nil
	}
	t.Cleanup(func() { registeredHost, installHost = oldReg, oldInstall })
	return &installed
}

func TestBrowserHostFollowsAGoneCopy(t *testing.T) {
	dir := t.TempDir()
	here := filepath.Join(dir, "new", cliName())
	there := filepath.Join(dir, "old", cliName())
	if err := os.MkdirAll(filepath.Dir(there), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(there, nil, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, reg, host string
		want            []string
	}{
		{"not installed", "", here, nil},
		{"installed for this copy", here, here, nil},
		{"the other copy is still there", there, here, nil},
		{"no command line program here", filepath.Join(dir, "gone", cliName()), "", nil},
		{"the other copy is gone", filepath.Join(dir, "gone", cliName()), here, []string{here}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installed := fakeRegistration(t, c.reg)
			followBrowserHost(c.host)
			if len(*installed) != len(c.want) || (len(c.want) > 0 && (*installed)[0] != c.want[0]) {
				t.Errorf("installed for %q, want %q", *installed, c.want)
			}
		})
	}
}
