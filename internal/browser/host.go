package browser

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// HostName is the native messaging host's name, the one the extension asks
// for and the browsers' registration names.
const HostName = "io.github.mustafaberatdemirci.siphon"

// FirefoxID is the extension's id in Firefox (its manifest's
// browser_specific_settings.gecko.id).
const FirefoxID = "siphon@mustafaberatdemirci.github.io"

// ChromeID is the extension's id in Chromium browsers: fixed by the public
// key in browser/manifest.json, so an unpacked copy has it wherever it lies.
const ChromeID = "jdjkkplekgegjommljilbmkddfiojlah"

// maxMessage bounds a message from the browser: a page's links fit easily.
const maxMessage = 8 << 20

// Request is a message from the extension.
type Request struct {
	// Type is "ping" (is Siphon there) or "add".
	Type string `json:"type"`
	// Links to add. With Page set they were collected from a whole page,
	// and only the ones a supported site recognizes are kept.
	Links    []string `json:"links"`
	Page     bool     `json:"page"`
	Referrer string   `json:"referrer"`
}

// Response answers a Request.
type Response struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Added   int    `json:"added"`
	Version string `json:"version,omitempty"`
}

// IsHostCall reports whether the program was started by a browser as its
// native messaging host. Chrome and its kin pass the caller's origin
// ("chrome-extension://<id>/"); Firefox passes the host manifest's path and
// the extension's id. The manifest can't add arguments of Siphon's own, so
// this is how the host mode is told apart.
func IsHostCall(args []string) bool {
	if len(args) >= 1 && strings.HasPrefix(args[0], "chrome-extension://") {
		return true
	}
	return len(args) >= 2 && strings.EqualFold(filepath.Ext(args[0]), ".json") && args[1] == FirefoxID
}

// Serve reads requests from the browser on r and writes one answer for each
// on w, until the browser closes r. A message that isn't JSON gets an error
// answer; one too large to be a request ends it.
func Serve(r io.Reader, w io.Writer, handle func(Request) Response) error {
	for {
		msg, err := readMessage(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var req Request
		resp := Response{Error: "the message isn't JSON"}
		if json.Unmarshal(msg, &req) == nil {
			resp = handle(req)
		}
		if err := writeMessage(w, resp); err != nil {
			return err
		}
	}
}

// readMessage reads one message: a 4-byte length in the machine's byte order
// (little endian on every system Siphon runs on), then that many bytes.
func readMessage(r io.Reader) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	if n > maxMessage {
		return nil, fmt.Errorf("a message of %d bytes is too large", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(b))); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
