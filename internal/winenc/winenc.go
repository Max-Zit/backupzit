// Package winenc reads the output of Windows console programs. They write
// in the console code page (e.g. 932 on Japanese Windows), not in UTF-8, so
// names with non-Latin letters arrive garbled when taken as UTF-8.
package winenc

import "unicode/utf8"

// Text returns the output b as a string: unchanged when it is valid UTF-8
// (or not on Windows), otherwise converted from the console code page.
func Text(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return fromCodePage(b)
}
