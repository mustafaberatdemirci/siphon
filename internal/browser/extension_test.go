package browser

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The extension in browser/ and the host registration must agree, or the
// extension silently can't reach Siphon: the id Chrome derives from the
// manifest's key is the one the host allows, Firefox's id is too, and the
// scripts ask for this host.
func TestExtensionMatchesTheHost(t *testing.T) {
	dir := filepath.Join("..", "..", "browser")
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Key         string   `json:"key"`
		Permissions []string `json:"permissions"`
		Background  struct {
			ServiceWorker string   `json:"service_worker"`
			Scripts       []string `json:"scripts"`
		} `json:"background"`
		Settings struct {
			Gecko struct {
				ID string `json:"id"`
			} `json:"gecko"`
		} `json:"browser_specific_settings"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(m.Key)
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
	if m.Settings.Gecko.ID != FirefoxID {
		t.Errorf("Firefox id %q, the host allows %q", m.Settings.Gecko.ID, FirefoxID)
	}
	if !strings.Contains(strings.Join(m.Permissions, " "), "nativeMessaging") {
		t.Error("the extension can't talk to a native host without nativeMessaging")
	}
	for _, f := range []string{m.Background.ServiceWorker, "options.js"} {
		src, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), `"`+HostName+`"`) {
			t.Errorf("%s doesn't ask for the host %s", f, HostName)
		}
	}
	if len(m.Background.Scripts) != 1 || m.Background.Scripts[0] != m.Background.ServiceWorker {
		t.Errorf("Firefox's background scripts %v and Chrome's service worker %q must be the same file", m.Background.Scripts, m.Background.ServiceWorker)
	}

	icon, err := os.ReadFile(filepath.Join(dir, "icon.png"))
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
