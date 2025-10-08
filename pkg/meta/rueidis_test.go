package meta

import "testing"

// This test codifies the Phase 0 expectation that Rueidis schemes are wired into
// the metadata driver registry. It deliberately fails until the Rueidis driver
// skeleton is added in Phase 1.
func TestRueidisDriverRegistered(t *testing.T) {
	required := []string{"rueidis", "ruediss"}
	for _, name := range required {
		name := name // capture
		t.Run(name, func(t *testing.T) {
			if _, ok := metaDrivers[name]; !ok {
				t.Fatalf("meta driver %q not registered; add registration before enabling Rueidis tests", name)
			}
		})
	}
}
