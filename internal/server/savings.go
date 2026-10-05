package server

import "fmt"

// compressionRatio shows how much compression shrank new data, e.g. "2.4×".
func compressionRatio(raw, stored uint64) string {
	if raw == 0 || stored == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f×", float64(raw)/float64(stored))
}

// dedupSaved is the share of the data read that was already in the
// repository (deduplication), e.g. "93 %".
func dedupSaved(read, added uint64) string {
	if read == 0 || added >= read {
		return "0 %"
	}
	return fmt.Sprintf("%.0f %%", 100*float64(read-added)/float64(read))
}
