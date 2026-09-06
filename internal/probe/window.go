// Package probe is a throwaway used to prove the prior-feedback path end to
// end. It is not part of firstpass and will be deleted.
package probe

// Average returns the mean of xs.
func Average(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total / len(xs)
}

// Scale multiplies every element by f, in place.
func Scale(xs []int, f int) {
	for i := range xs {
		xs[i] = xs[i] * f
	}
}
