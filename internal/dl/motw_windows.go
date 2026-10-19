package dl

import "os"

// MarkDownloaded tags a finished download as coming from the internet, the
// way browsers do (the "Mark of the Web", a Zone.Identifier stream): Windows
// then warns before starting a downloaded program and Office opens a
// downloaded document in Protected View. source is the page it came from.
//
// Failing is not an error: a file system without streams (FAT32, exFAT, a
// network share) simply keeps the file unmarked.
func MarkDownloaded(path, source string) {
	content := "[ZoneTransfer]\r\nZoneId=3\r\n"
	if u := webAddress(source); u != "" {
		content += "ReferrerUrl=" + u + "\r\nHostUrl=" + u + "\r\n"
	}
	_ = os.WriteFile(path+":Zone.Identifier", []byte(content), 0o644)
}
