package version

import "testing"

func TestVersion_IsNonEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must be non-empty")
	}
}
