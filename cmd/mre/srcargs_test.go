package main

import (
	"reflect"
	"testing"
)

// TestAdapterSrcArgs checks the -srcargs flag value is split into the adapter's
// SrcArgs (whitespace-separated), so a comp stage runs `mrjob cr_lib martian
// <stage> ...`. Empty -srcargs yields no args (py/exec stages).
func TestAdapterSrcArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"martian align_and_count", []string{"martian", "align_and_count"}},
		{"  martian   x  ", []string{"martian", "x"}},
		{"", []string{}},
	}

	for _, c := range cases {
		cf := &commonFlags{lang: "comp", srcargs: c.in}
		if got := cf.adapter().SrcArgs; !reflect.DeepEqual(got, c.want) {
			t.Errorf("srcargs %q -> SrcArgs %v, want %v", c.in, got, c.want)
		}
	}
}
