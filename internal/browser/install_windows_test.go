package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// Install registers the host with every browser under HKEY_CURRENT_USER (in
// a test key here), each pointing at the manifest of its kind; Uninstall
// takes it all away.
func TestInstallWindows(t *testing.T) {
	cfg := t.TempDir()
	userConfigDir = func() (string, error) { return cfg, nil }
	regBase = fmt.Sprintf(`Software\SiphonTest%d`, time.Now().UnixNano())
	t.Cleanup(func() {
		userConfigDir = os.UserConfigDir
		deleteTree(registry.CURRENT_USER, regBase)
		regBase = `Software`
	})
	host := filepath.Join(t.TempDir(), "siphon.exe")
	if err := os.WriteFile(host, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Install(host)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(windowsBrowsers) {
		t.Errorf("registered with %v", got)
	}
	for _, b := range windowsBrowsers {
		k, err := registry.OpenKey(registry.CURRENT_USER, hostKey(b.key), registry.QUERY_VALUE)
		if err != nil {
			t.Fatalf("%s: %v", b.name, err)
		}
		file, _, err := k.GetStringValue("")
		k.Close()
		if err != nil {
			t.Fatal(err)
		}
		var m manifest
		data, _ := os.ReadFile(file)
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: %v", b.name, err)
		}
		if m.Name != HostName || m.Path != host || m.Type != "stdio" {
			t.Errorf("%s: manifest %+v", b.name, m)
		}
		if b.firefox != (len(m.AllowedExtensions) == 1 && m.AllowedExtensions[0] == FirefoxID && m.AllowedOrigins == nil) ||
			!b.firefox && (len(m.AllowedOrigins) != 1 || m.AllowedOrigins[0] != "chrome-extension://"+ChromeID+"/") {
			t.Errorf("%s: allowed callers %+v", b.name, m)
		}
	}
	if Registered() != host {
		t.Errorf("Registered = %q", Registered())
	}

	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	if Registered() != "" {
		t.Error("still registered after Uninstall")
	}
}

func TestInstallNeedsTheHost(t *testing.T) {
	if _, err := Install(filepath.Join(t.TempDir(), "missing.exe")); err == nil {
		t.Error("installed a host that isn't there")
	}
	if _, err := Install("siphon.exe"); err == nil {
		t.Error("installed a relative path")
	}
}

func deleteTree(root registry.Key, path string) {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS)
	if err == nil {
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, n := range names {
			deleteTree(root, path+`\`+n)
		}
	}
	_ = registry.DeleteKey(root, path)
}
