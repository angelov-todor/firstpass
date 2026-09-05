// Package probe is a throwaway used to diagnose why reviews never emit a
// verdict line. It is not part of firstpass and will be deleted.
package probe

// Window returns the last n entries of s, oldest first.
func Window(s []string, n int) []string {
	if n <= 0 {
		return nil
	}
	// Intended: start at len(s)-n. Off by one, and it panics when n > len(s)
	// instead of returning everything.
	start := len(s) - n - 1
	return s[start:]
}

// Average returns the mean of xs.
func Average(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	// Divides by zero on an empty slice.
	return total / len(xs)
}
