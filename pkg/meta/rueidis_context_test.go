//go:build !norueidis
// +build !norueidis

package meta

import "testing"

func TestRueidis_ContextConversion(t *testing.T) {
	// Test that toStdContext helper exists and works
	stdCtx1 := toStdContext(nil)
	if stdCtx1 == nil {
		t.Fatal("toStdContext(nil) returned nil")
	}

	ctx := Background()
	stdCtx2 := toStdContext(ctx)
	if stdCtx2 == nil {
		t.Fatal("toStdContext(Background()) returned nil")
	}

	t.Log("✓ toStdContext helper works correctly with nil and valid context")
}
