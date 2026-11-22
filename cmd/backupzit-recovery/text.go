package main

import (
	"fmt"
	"os"
	"strings"
)

// lang is "de" or "en". The recovery program is German and English; the
// texts sit next to each other in the code (t("Deutsch", "English")).
var lang = detectLang()

func detectLang() string {
	for _, a := range os.Args[1:] {
		if a == "--lang=en" || a == "--en" {
			return "en"
		}
		if a == "--lang=de" || a == "--de" {
			return "de"
		}
	}
	if v := os.Getenv("BACKUPZIT_LANG"); v != "" {
		if strings.HasPrefix(strings.ToLower(v), "de") {
			return "de"
		}
		return "en"
	}
	return systemLang()
}

// t picks the text in the current language.
func t(de, en string) string {
	if lang == "de" {
		return de
	}
	return en
}

// tf is t with fmt.Sprintf.
func tf(de, en string, args ...any) string { return fmt.Sprintf(t(de, en), args...) }

// humanBytes formats a size, e.g. "59.4 GB".
func humanBytes(b uint64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	s := fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
	if lang == "de" {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s
}

// gib converts bytes to GiB for size fields, and back.
func gib(b uint64) float64     { return float64(b) / (1 << 30) }
func fromGiB(g float64) uint64 { return uint64(g * (1 << 30)) }
