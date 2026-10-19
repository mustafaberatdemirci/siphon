package browser

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// regBase is where the browsers' keys live under HKEY_CURRENT_USER; tests
// point it at a key of their own.
var regBase = `Software`

// windowsBrowsers: each browser's key under regBase, and whether it reads
// the Firefox manifest.
var windowsBrowsers = []struct {
	name, key string
	firefox   bool
}{
	{"Chrome", `Google\Chrome`, false},
	{"Edge", `Microsoft\Edge`, false},
	{"Brave", `BraveSoftware\Brave-Browser`, false},
	{"Chromium", `Chromium`, false},
	{"Firefox", `Mozilla`, true},
}

func hostKey(browserKey string) string {
	return regBase + `\` + browserKey + `\NativeMessagingHosts\` + HostName
}

// manifestDir is where the manifests are written on Windows: the registry
// keys point at them.
func manifestDir() (string, error) {
	base, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Siphon", "browser"), nil
}

func install(host string) ([]string, error) {
	dir, err := manifestDir()
	if err != nil {
		return nil, err
	}
	chrome := filepath.Join(dir, HostName+".chrome.json")
	firefox := filepath.Join(dir, HostName+".firefox.json")
	if err := writeManifest(chrome, chromeManifest(host)); err != nil {
		return nil, err
	}
	if err := writeManifest(firefox, firefoxManifest(host)); err != nil {
		return nil, err
	}
	var done []string
	for _, b := range windowsBrowsers {
		file := chrome
		if b.firefox {
			file = firefox
		}
		k, _, err := registry.CreateKey(registry.CURRENT_USER, hostKey(b.key), registry.SET_VALUE)
		if err != nil {
			return done, err
		}
		err = k.SetStringValue("", file)
		k.Close()
		if err != nil {
			return done, err
		}
		done = append(done, b.name)
	}
	return done, nil
}

func uninstall() error {
	var errs []error
	for _, b := range windowsBrowsers {
		if err := registry.DeleteKey(registry.CURRENT_USER, hostKey(b.key)); err != nil && !errors.Is(err, registry.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if dir, err := manifestDir(); err == nil {
		_ = os.RemoveAll(dir)
	}
	return errors.Join(errs...)
}

func registered() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, hostKey(windowsBrowsers[0].key), registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	file, _, err := k.GetStringValue("")
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	var m manifest
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m.Path
}
