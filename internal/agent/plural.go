package agent

import "fmt"

// count writes n with the singular or plural noun: "1 database", "2 databases".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
