package browser

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type extManifest struct {
	Version     string   `json:"version"`
	Key         string   `json:"key"`
	Permissions []string `json:"permissions"`
	Background  struct {
		ServiceWorker string   `json:"service_worker"`
		Scripts       []string `json:"scripts"`
	} `json:"background"`
	Settings *struct {
		Gecko struct {
			ID string `json:"id"`
		} `json:"gecko"`
	} `json:"browser_specific_settings"`
}

var extDir = filepath.Join("..", "..", "browser")

func readManifest(t *testing.T, name string) extManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(extDir, "manifest."+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m extManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

// The extension in browser/ and the host registration must agree, or the
// extension silently can't reach Siphon: the id Chrome derives from the
// Chromium manifest's key is the one the host allows, Firefox's id is too,
// and the scripts ask for this host. Each browser gets only the background
// key it reads; the other one's key is a warning on its extensions page.
func TestExtensionMatchesTheHost(t *testing.T) {
	chrome, firefox := readManifest(t, "chromium"), readManifest(t, "firefox")

	der, err := base64.StdEncoding.DecodeString(chrome.Key)
	if err != nil {
		t.Fatal(err)
	}
	// Chrome's id: the first 16 bytes of the key's SHA-256, hex digits 0-f
	// written as letters a-p.
	sum := sha256.Sum256(der)
	id := []byte(hex.EncodeToString(sum[:16]))
	for i, c := range id {
		if c <= '9' {
			id[i] = 'a' + c - '0'
		} else {
			id[i] = 'a' + c - 'a' + 10
		}
	}
	if string(id) != ChromeID {
		t.Errorf("the manifest's key gives the id %s, the host allows %s", id, ChromeID)
	}
	if chrome.Background.ServiceWorker == "" || chrome.Background.Scripts != nil || chrome.Settings != nil {
		t.Errorf("Chromium manifest: background %+v, Firefox settings %v", chrome.Background, chrome.Settings != nil)
	}
	if firefox.Settings == nil || firefox.Settings.Gecko.ID != FirefoxID {
		t.Errorf("Firefox id: %+v, the host allows %q", firefox.Settings, FirefoxID)
	}
	if len(firefox.Background.Scripts) != 1 || firefox.Background.ServiceWorker != "" || firefox.Key != "" {
		t.Errorf("Firefox manifest: background %+v, key set %v", firefox.Background, firefox.Key != "")
	}
	if firefox.Background.Scripts[0] != chrome.Background.ServiceWorker {
		t.Error("the two manifests start different background scripts")
	}
	if chrome.Version != firefox.Version || !slices.Equal(chrome.Permissions, firefox.Permissions) {
		t.Errorf("the manifests differ: %s %v / %s %v", chrome.Version, chrome.Permissions, firefox.Version, firefox.Permissions)
	}
	if !slices.Contains(chrome.Permissions, "nativeMessaging") {
		t.Error("the extension can't talk to a native host without nativeMessaging")
	}

	pack, err := os.ReadFile(filepath.Join(extDir, "pack.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{chrome.Background.ServiceWorker, "options.html", "options.js", "icon.png"} {
		if !strings.Contains(string(pack), "/"+f+`"`) {
			t.Errorf("pack.sh doesn't copy %s", f)
		}
	}
	for _, f := range []string{chrome.Background.ServiceWorker, "options.js"} {
		src, err := os.ReadFile(filepath.Join(extDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), `"`+HostName+`"`) {
			t.Errorf("%s doesn't ask for the host %s", f, HostName)
		}
	}

	icon, err := os.ReadFile(filepath.Join(extDir, "icon.png"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := os.ReadFile(filepath.Join("..", "..", "docs", "icon.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(icon, app) {
		t.Error("browser/icon.png differs from docs/icon.png; copy it over")
	}
}
