package ids

import (
	"regexp"
	"testing"
)

func TestNewIsAULIDWithThePrefix(t *testing.T) {
	re := regexp.MustCompile(`^sg_[0-9A-HJKMNP-TV-Z]{26}$`)
	seen := map[string]bool{}
	for range 1000 {
		id := New("sg_")
		if !re.MatchString(id) || seen[id] {
			t.Fatalf("id %q (seen before: %v)", id, seen[id])
		}
		seen[id] = true
	}
}
