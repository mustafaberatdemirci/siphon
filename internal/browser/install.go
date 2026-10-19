package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Installing the browser integration registers Siphon's command line program
// as the native messaging host of every Chromium browser and Firefox: a small
// JSON manifest naming the program and the one extension allowed to start it.
// Only the Siphon extension can reach the host; web pages can't.

// userConfigDir and homeDir are os.UserConfigDir and os.UserHomeDir; tests
// point them elsewhere.
var (
	userConfigDir = os.UserConfigDir
	homeDir       = os.UserHomeDir
)

type manifest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedOrigins    []string `json:"allowed_origins,omitempty"`
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
}

// chromeManifest and firefoxManifest differ only in how they name the
// extension; each browser refuses the other's field.
func chromeManifest(host string) manifest {
	return manifest{Name: HostName, Description: "Siphon", Path: host, Type: "stdio",
		AllowedOrigins: []string{"chrome-extension://" + ChromeID + "/"}}
}

func firefoxManifest(host string) manifest {
	return manifest{Name: HostName, Description: "Siphon", Path: host, Type: "stdio",
		AllowedExtensions: []string{FirefoxID}}
}

func writeManifest(path string, m manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Install registers host (the absolute path of Siphon's command line
// program) with the browsers and returns their names.
func Install(host string) ([]string, error) {
	if !filepath.IsAbs(host) {
		return nil, fmt.Errorf("the host must be an absolute path: %s", host)
	}
	if fi, err := os.Stat(host); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("Siphon's command line program isn't at %s", host)
	}
	return install(host)
}

// Uninstall takes the registration away from every browser.
func Uninstall() error { return uninstall() }

// Registered reports the host program the browsers would start ("" if the
// integration isn't installed).
func Registered() string { return registered() }

// errNoBrowser says no supported browser was found to register with.
var errNoBrowser = errors.New("no Chrome, Edge, Brave, Chromium or Firefox profile was found")
