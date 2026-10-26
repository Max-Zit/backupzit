package winenc

import (
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// cpOEM is the OEM code page that console programs use without a console
// of their own (as under the agent service).
const cpOEM = 1

func fromCodePage(b []byte) string {
	return decode(cpOEM, b)
}

// decode converts b from code page cp; on failure b is returned as is.
func decode(cp uint32, b []byte) string {
	if len(b) == 0 {
		return ""
	}
	n, err := windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), nil, 0)
	if err != nil || n <= 0 {
		return string(b)
	}
	w := make([]uint16, n)
	if n, err = windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), &w[0], n); err != nil {
		return string(b)
	}
	return string(utf16.Decode(w[:n]))
}
