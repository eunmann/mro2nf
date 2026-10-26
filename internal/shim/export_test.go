package shim

// CountResolves counts the real paths of r's roots that r and its copies
// read from now on.
func CountResolves(r Roots) *int {
	n := new(int)
	read := r.real.read
	r.real.read = func(p string) (string, error) {
		*n++

		return read(p)
	}

	return n
}
