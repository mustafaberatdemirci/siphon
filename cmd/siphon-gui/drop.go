package main

// Dropping files on the window: the links in them open the Add dialog. A
// list of links (.txt), a browser's link shortcut (.url, what dragging a
// link to the desktop makes) or a saved page (.html, its supported sites'
// links only: a page links to everything).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"

	"github.com/mustafaberatdemirci/siphon/internal/browser"
)

// maxDropFile: a larger file isn't a list of links.
const maxDropFile = 4 << 20

var errNotLinkFile = errors.New("not a file of links")

// linksInFile reads the links of a dropped file. recognized tells the
// supported sites' links, the only ones taken from a page.
func linksInFile(path string, recognized func(string) bool) ([]string, error) {
	pageOnly := false
	switch strings.ToLower(filepath.Ext(path)) {
	case ".txt", ".url", ".lst", ".list", ".csv", ".md", ".log", "":
	case ".html", ".htm", ".mhtml":
		pageOnly = true
	default:
		return nil, errNotLinkFile
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() || fi.Size() > maxDropFile {
		return nil, errNotLinkFile
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range browser.ExtractLinks(string(b)) {
		if !pageOnly || recognized(l) {
			out = append(out, l)
		}
	}
	return out, nil
}

// onDropped is the window's drop handler. UI thread.
func (q *queueTab) onDropped(_ fyne.Position, uris []fyne.URI) {
	var links, empty []string
	for _, u := range uris {
		if u.Scheme() != "file" {
			continue
		}
		found, err := linksInFile(u.Path(), q.eng.Recognizes)
		if err != nil || len(found) == 0 {
			empty = append(empty, filepath.Base(u.Path()))
			continue
		}
		links = append(links, found...)
	}
	if len(empty) > 0 {
		q.vm.Notify(Tf("No links found in %s. Siphon takes links from text files (.txt), link shortcuts (.url) and saved pages (.html).", strings.Join(empty, ", ")))
	}
	q.receiveLinks(links)
}
