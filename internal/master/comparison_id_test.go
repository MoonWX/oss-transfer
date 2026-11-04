package master

import "testing"

func TestComparisonIDNeverStartsWithDash(t *testing.T) {
	for i := 0; i < 2000; i++ {
		id, err := comparisonID()
		if err != nil || id == "" || id[0] == '-' {
			t.Fatalf("comparisonID() = %q, %v", id, err)
		}
	}
}
