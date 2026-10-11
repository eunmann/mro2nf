//go:build e2e

package e2e

import "testing"

// TestRemovingAnOutcomeEntryFailsItsCase replays each mrpOutcomeCases entry
// that names a divergence on its recorded outcomes, mrp's from the case and
// mro2nf's from the allowlist entry: the case holds with the entry, and fails
// once the entry is removed, whether or not the case still names it.
func TestRemovingAnOutcomeEntryFailsItsCase(t *testing.T) {
	allow := mustAllowlist(t)

	for _, tc := range mrpOutcomeCases {
		if tc.divergence == "" {
			continue
		}

		t.Run(tc.name, func(t *testing.T) {
			d, err := tc.validate(allow)
			if err != nil {
				t.Fatal(err)
			}

			if bad := tc.check(d, tc.mrp, d.nf); len(bad) > 0 {
				t.Errorf("the recorded outcomes fail the case: %q", bad)
			}

			if _, err := tc.validate(allow.Without(tc.divergence)); err == nil {
				t.Errorf("the case still validates without entry %s", tc.divergence)
			}

			if bad := tc.check(nil, tc.mrp, d.nf); len(bad) == 0 {
				t.Errorf("with no divergence the recorded outcomes still pass the case")
			}
		})
	}
}
