package shim

// CountResolves counts the real paths of r's roots, its inputs' and its
// code's, that r and its copies read from now on.
func CountResolves(r Roots) *int {
	n := new(int)

	for _, s := range []rootSet{r.input, r.code} {
		if s.real == nil {
			continue
		}

		read := s.real.read
		s.real.read = func(p string) (string, error) {
			*n++

			return read(p)
		}
	}

	return n
}
