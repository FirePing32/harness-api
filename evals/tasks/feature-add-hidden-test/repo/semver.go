// Package semver compares semantic version strings.
package semver

// Compare orders two semantic versions.
//
// It returns -1 if a sorts before b, +1 if a sorts after b, and 0 if they are
// equal. Versions look like "1.4.2" or "v1.4.2"; a leading "v" is optional and
// is not part of the comparison. Each of the three components is compared
// numerically, not as text, so "1.10.0" sorts after "1.9.0".
//
// A version with fewer than three components is padded with zeroes, so "1.2"
// equals "1.2.0". Anything that cannot be parsed sorts before everything else.
func Compare(a, b string) int {
	panic("not implemented")
}
