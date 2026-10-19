//go:build !windows

package browser

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// otherBrowsers: each browser's profile folder (relative to home) and the
// folder in it where native messaging host manifests go.
type browserDir struct {
	name, root, hosts string
	firefox           bool
}

func otherBrowsers() []browserDir {
	if runtime.GOOS == "darwin" {
		as := filepath.Join("Library", "Application Support")
		return []browserDir{
			{"Chrome", filepath.Join(as, "Google", "Chrome"), "NativeMessagingHosts", false},
			{"Edge", filepath.Join(as, "Microsoft Edge"), "NativeMessagingHosts", false},
			{"Brave", filepath.Join(as, "BraveSoftware", "Brave-Browser"), "NativeMessagingHosts", false},
			{"Chromium", filepath.Join(as, "Chromium"), "NativeMessagingHosts", false},
			{"Firefox", filepath.Join(as, "Mozilla"), "NativeMessagingHosts", true},
		}
	}
	return []browserDir{
		{"Chrome", filepath.Join(".config", "google-chrome"), "NativeMessagingHosts", false},
		{"Edge", filepath.Join(".config", "microsoft-edge"), "NativeMessagingHosts", false},
		{"Brave", filepath.Join(".config", "BraveSoftware", "Brave-Browser"), "NativeMessagingHosts", false},
		{"Chromium", filepath.Join(".config", "chromium"), "NativeMessagingHosts", false},
		{"Firefox", ".mozilla", "native-messaging-hosts", true},
	}
}

// install writes the manifest into the profile folder of every browser
// that has one: a folder isn't made for a browser that isn't there.
func install(host string) ([]string, error) {
	home, err := homeDir()
	if err != nil {
		return nil, err
	}
	var done []string
	for _, b := range otherBrowsers() {
		if fi, err := os.Stat(filepath.Join(home, b.root)); err != nil || !fi.IsDir() {
			continue
		}
		m := chromeManifest(host)
		if b.firefox {
			m = firefoxManifest(host)
		}
		if err := writeManifest(filepath.Join(home, b.root, b.hosts, HostName+".json"), m); err != nil {
			return done, err
		}
		done = append(done, b.name)
	}
	if len(done) == 0 {
		return nil, errNoBrowser
	}
	return done, nil
}

func uninstall() error {
	home, err := homeDir()
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range otherBrowsers() {
		if err := os.Remove(filepath.Join(home, b.root, b.hosts, HostName+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func registered() string {
	home, err := homeDir()
	if err != nil {
		return ""
	}
	for _, b := range otherBrowsers() {
		data, err := os.ReadFile(filepath.Join(home, b.root, b.hosts, HostName+".json"))
		if err != nil {
			continue
		}
		var m manifest
		if json.Unmarshal(data, &m) == nil && m.Path != "" {
			return m.Path
		}
	}
	return ""
}
