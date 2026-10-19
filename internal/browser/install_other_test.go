//go:build !windows

package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Install writes the manifest into the profile folder of each browser that
// has one, and makes no folder for a browser that isn't there.
func TestInstallFiles(t *testing.T) {
	home := t.TempDir()
	homeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { homeDir = os.UserHomeDir })
	bs := otherBrowsers()
	chrome, firefox, absent := bs[0], bs[4], bs[1]
	for _, b := range []browserDir{chrome, firefox} {
		if err := os.MkdirAll(filepath.Join(home, b.root), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	host := filepath.Join(t.TempDir(), "siphon")
	if err := os.WriteFile(host, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Install(host)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("registered with %v, want Chrome and Firefox", got)
	}
	for _, b := range []browserDir{chrome, firefox} {
		var m manifest
		data, err := os.ReadFile(filepath.Join(home, b.root, b.hosts, HostName+".json"))
		if err != nil || json.Unmarshal(data, &m) != nil || m.Path != host {
			t.Errorf("%s: %v %+v", b.name, err, m)
		}
		if b.firefox && (len(m.AllowedExtensions) != 1 || m.AllowedOrigins != nil) {
			t.Errorf("%s: %+v", b.name, m)
		}
	}
	if _, err := os.Stat(filepath.Join(home, absent.root)); !os.IsNotExist(err) {
		t.Errorf("a folder was made for %s, which isn't there", absent.name)
	}
	if Registered() != host {
		t.Errorf("Registered = %q", Registered())
	}
	if err := Uninstall(); err != nil || Registered() != "" {
		t.Errorf("Uninstall: %v, still %q", err, Registered())
	}
}
