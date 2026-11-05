package wiki

// Cut keeps the first n characters of s.
func Cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Clip keeps the first n characters of s and ends with an ellipsis when it cut.
func Clip(s string, n int) string {
	if c := Cut(s, n); c != s {
		return c + "…"
	}
	return s
}
