//go:build !windows

package dl

// MarkDownloaded tags a finished download as coming from the internet. Only
// Windows has such a mark that the system acts on; elsewhere it does nothing.
func MarkDownloaded(path, source string) {}
