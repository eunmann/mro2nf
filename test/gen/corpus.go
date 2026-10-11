package gen

// The literal corpus: the value boundaries where mro2nf's encoding, binding or
// publishing has diverged from mrp's before, or plausibly could. The stub
// stages draw their outputs from the same kinds of values (stub.py).

// intTokens include integers past 2^53, where a float64 round trip loses
// precision, and a leading-zero token Martian reads as decimal.
func intTokens() []string {
	return []string{
		"0", "1", "-1", "42", "007",
		"9007199254740993", "-9007199254740993",
		"9223372036854775807", "-9223372036854775808",
	}
}

// floatTokens include whole floats >= 1e6, which JSON encoders write in
// exponent form (1e+06), floats <= 1e-6, negative zero, whole floats Martian
// may hand a stage as ints, and int tokens bound to a float.
func floatTokens() []string {
	return []string{
		"1e6", "2500000.0", "1e20", "1e21", "1.5e300",
		"1e-6", "1e-7", "1.5e-6", "5e-324",
		"-0.0", "0.0", "2.0", "3.0", "0.5", "-2.25", "123456789.125", "7",
	}
}

// stringValues include strings shaped like mre's own file markers and paths,
// escapes, unicode, and text that reads as another JSON type.
func stringValues() []string {
	return []string{
		"", "a", "hello world",
		"@mre:file:0/x.txt", "@mre:dir:d", "@mre:", "@mre:file:",
		"line1\nline2", "tab\tsep", `quote"q`, `back\slash`,
		"ünïcødé ✓", "€ \U0001F600", "01", "null", "true", "1e6",
		"/tmp/not-a-file", "a~b", "~",
	}
}

// mapKeys include the empty key, mre's composite-key separator (~), a
// newline, numeric-looking keys whose string and numeric orders differ, and
// non-ASCII keys: U+FF5E sorts after U+1F600 in UTF-8 but before it in
// UTF-16, the order a JVM compares strings in.
func mapKeys() []string {
	return []string{
		"", "~", "a~b", "\n", "é", "～", "\U0001F600", "01", "10", "2", "a", "B", "k k", "x.y",
	}
}

// fileTypes are user filetypes, multi-part extensions among them.
func fileTypes() []string { return []string{"txt", "json", "fastq.gz", "bam", "csv"} }
