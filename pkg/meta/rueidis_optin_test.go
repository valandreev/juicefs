//go:build !norueidis
// +build !norueidis

package meta

import (
	"strings"
	"testing"
	"time"
)

// TestOPTIN_URIParameterParsing tests that URI parameters are correctly parsed
// for subscription mode selection (OPTIN vs BCAST)
func TestOPTIN_URIParameterParsing(t *testing.T) {
	tests := []struct {
		name          string
		addr          string
		expectedMode  string
		expectedError bool
	}{
		{
			name:         "explicit optin mode",
			addr:         "rueidis://100.121.51.13:6379/0?subscribe=optin",
			expectedMode: "optin",
		},
		{
			name:         "explicit bcast mode",
			addr:         "rueidis://100.121.51.13:6379/0?subscribe=bcast",
			expectedMode: "bcast",
		},
		{
			name:         "default mode (no subscribe param)",
			addr:         "rueidis://100.121.51.13:6379/0",
			expectedMode: "optin", // default
		},
		{
			name:         "invalid subscribe value defaults to optin",
			addr:         "rueidis://100.121.51.13:6379/0?subscribe=invalid",
			expectedMode: "optin", // default on invalid
		},
		{
			name:         "subscribe with ttl parameter",
			addr:         "rueidis://100.121.51.13:6379/0?ttl=5s&subscribe=optin",
			expectedMode: "optin",
		},
		{
			name:         "subscribe with metaprime and batch params",
			addr:         "rueidis://100.121.51.13:6379/0?subscribe=bcast&metaprime=1&inode_batch=100",
			expectedMode: "bcast",
		},
		{
			name:         "case sensitive: Optin should default",
			addr:         "rueidis://100.121.51.13:6379/0?subscribe=Optin",
			expectedMode: "optin", // lowercase only
		},
		{
			name:         "multiple params with subscribe",
			addr:         "rueidis://100.121.51.13:6379/0?ttl=2h&subscribe=bcast&prime=1",
			expectedMode: "bcast",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.addr, &Config{Retries: 10})
			if client == nil {
				t.Fatalf("Failed to create client for %s", tt.name)
			}

			rm, ok := client.(*rueidisMeta)
			if !ok {
				t.Fatalf("Expected *rueidisMeta, got %T", client)
			}

			if rm.subscribeMode != tt.expectedMode {
				t.Errorf("Expected subscribeMode=%q, got %q", tt.expectedMode, rm.subscribeMode)
			}
		})
	}
}

// TestOPTIN_DefaultModeBCAST tests that OPTIN mode (default) has empty ClientTrackingOptions
func TestOPTIN_DefaultModeClientTracking(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/1"
	client := NewClient(addr, &Config{Retries: 10})
	if client == nil {
		t.Fatal("Failed to create client")
	}

	rm := client.(*rueidisMeta)

	// Verify subscribeMode is optin
	if rm.subscribeMode != "optin" {
		t.Errorf("Expected default subscribeMode='optin', got %q", rm.subscribeMode)
	}

	// For OPTIN mode, ClientTrackingOptions should be empty or nil
	if len(rm.option.ClientTrackingOptions) > 0 {
		t.Errorf("Expected empty ClientTrackingOptions for OPTIN mode, got %v",
			rm.option.ClientTrackingOptions)
	}
}

// TestOPTIN_ExplicitBCASTMode tests that BCAST mode has proper ClientTrackingOptions
func TestOPTIN_ExplicitBCASTModeClientTracking(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/2?subscribe=bcast"
	client := NewClient(addr, &Config{Retries: 10})
	if client == nil {
		t.Fatal("Failed to create client")
	}

	rm := client.(*rueidisMeta)

	// Verify subscribeMode is bcast
	if rm.subscribeMode != "bcast" {
		t.Errorf("Expected subscribeMode='bcast', got %q", rm.subscribeMode)
	}

	// For BCAST mode, ClientTrackingOptions should be set
	if len(rm.option.ClientTrackingOptions) == 0 {
		t.Error("Expected non-empty ClientTrackingOptions for BCAST mode")
	}

	// Verify BCAST option is present
	hasBcast := false
	for _, opt := range rm.option.ClientTrackingOptions {
		if opt == "BCAST" {
			hasBcast = true
			break
		}
	}
	if !hasBcast {
		t.Error("Expected BCAST option in ClientTrackingOptions")
	}

	// Verify PREFIX options are present
	hasPrefixes := false
	for _, opt := range rm.option.ClientTrackingOptions {
		if opt == "PREFIX" {
			hasPrefixes = true
			break
		}
	}
	if !hasPrefixes {
		t.Error("Expected PREFIX options in ClientTrackingOptions for metadata key tracking")
	}
}

// TestOPTIN_BCASTModePrefixes tests that BCAST mode tracks correct key prefixes
func TestOPTIN_BCASTModePrefixes(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/3?subscribe=bcast"
	client := NewClient(addr, &Config{Retries: 10})
	if client == nil {
		t.Fatal("Failed to create client")
	}

	rm := client.(*rueidisMeta)

	// Expected prefixes for metadata keys
	expectedPrefixes := map[string]bool{
		"i": false, // inode keys
		"d": false, // directory keys
		"c": false, // chunk keys
		"x": false, // xattr keys
		"p": false, // parent keys
		"s": false, // symlink keys
	}

	// Check if prefixes are in ClientTrackingOptions
	for i := 0; i < len(rm.option.ClientTrackingOptions)-1; i++ {
		if rm.option.ClientTrackingOptions[i] == "PREFIX" {
			prefixValue := rm.option.ClientTrackingOptions[i+1]
			// Extract the suffix (everything after prefix)
			for suffix := range expectedPrefixes {
				if strings.HasSuffix(prefixValue, suffix) {
					expectedPrefixes[suffix] = true
					break
				}
			}
		}
	}

	// Verify all expected prefixes are present
	for prefix, found := range expectedPrefixes {
		if !found {
			t.Errorf("Expected prefix tracking for '%s' keys not found", prefix)
		}
	}
}

// TestOPTIN_SubscribeModeFieldAccessible tests that subscribeMode is properly stored
// and accessible for debugging/metrics
func TestOPTIN_SubscribeModeFieldAccessible(t *testing.T) {
	tests := []struct {
		name         string
		addr         string
		expectedMode string
	}{
		{"optin explicit", "rueidis://100.121.51.13:6379/4?subscribe=optin", "optin"},
		{"bcast explicit", "rueidis://100.121.51.13:6379/4?subscribe=bcast", "bcast"},
		{"default", "rueidis://100.121.51.13:6379/4", "optin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.addr, &Config{Retries: 10})
			if client == nil {
				t.Fatalf("Failed to create client")
			}

			rm := client.(*rueidisMeta)

			// Test subscribeMode is accessible
			if rm.subscribeMode == "" {
				t.Error("subscribeMode should not be empty")
			}

			// Test subscribeMode matches expected
			if rm.subscribeMode != tt.expectedMode {
				t.Errorf("Expected subscribeMode=%q, got %q", tt.expectedMode, rm.subscribeMode)
			}

			// Test it can be used for logging/debugging
			modeStr := "subscribe_mode=" + rm.subscribeMode
			if !strings.Contains(modeStr, tt.expectedMode) {
				t.Errorf("Mode not properly formatted for logging: %s", modeStr)
			}
		})
	}
}

// TestOPTIN_CombinedWithOtherParameters tests that subscribe mode works with other URI params
func TestOPTIN_CombinedWithOtherParameters(t *testing.T) {
	tests := []struct {
		name              string
		addr              string
		expectedMode      string
		expectedTTL       time.Duration
		expectedMetaprime bool
	}{
		{
			name:              "optin with 5s ttl",
			addr:              "rueidis://100.121.51.13:6379/5?subscribe=optin&ttl=5s",
			expectedMode:      "optin",
			expectedTTL:       5 * time.Second,
			expectedMetaprime: false,
		},
		{
			name:              "bcast with 2h ttl",
			addr:              "rueidis://100.121.51.13:6379/5?subscribe=bcast&ttl=2h",
			expectedMode:      "bcast",
			expectedTTL:       2 * time.Hour,
			expectedMetaprime: false,
		},
		{
			name:              "optin with metaprime",
			addr:              "rueidis://100.121.51.13:6379/5?subscribe=optin&metaprime=1",
			expectedMode:      "optin",
			expectedTTL:       1 * time.Hour, // default
			expectedMetaprime: true,
		},
		{
			name:              "bcast with batch params",
			addr:              "rueidis://100.121.51.13:6379/5?subscribe=bcast&inode_batch=100&chunk_batch=500",
			expectedMode:      "bcast",
			expectedTTL:       1 * time.Hour, // default
			expectedMetaprime: true,          // auto-enabled by batch params
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.addr, &Config{Retries: 10})
			if client == nil {
				t.Fatalf("Failed to create client")
			}

			rm := client.(*rueidisMeta)

			// Check subscribeMode
			if rm.subscribeMode != tt.expectedMode {
				t.Errorf("Expected subscribeMode=%q, got %q", tt.expectedMode, rm.subscribeMode)
			}

			// Check TTL
			if rm.cacheTTL != tt.expectedTTL {
				t.Errorf("Expected cacheTTL=%v, got %v", tt.expectedTTL, rm.cacheTTL)
			}

			// Check metaprime
			if rm.metaPrimeEnabled != tt.expectedMetaprime {
				t.Errorf("Expected metaPrimeEnabled=%v, got %v", tt.expectedMetaprime, rm.metaPrimeEnabled)
			}
		})
	}
}

// TestOPTIN_ClientTrackingOptionsCleanupForRueidis tests that subscribe param
// is properly stripped before passing to Rueidis
func TestOPTIN_ClientTrackingOptionsCleanupForRueidis(t *testing.T) {
	// This test verifies that the subscribe parameter doesn't cause
	// "unexpected option: subscribe" error from Rueidis ParseURL
	tests := []struct {
		name string
		addr string
	}{
		{"optin param", "rueidis://100.121.51.13:6379/6?subscribe=optin"},
		{"bcast param", "rueidis://100.121.51.13:6379/6?subscribe=bcast"},
		{"optin with other params", "rueidis://100.121.51.13:6379/6?ttl=5s&subscribe=optin&prime=1"},
		{"bcast first", "rueidis://100.121.51.13:6379/6?subscribe=bcast&ttl=2h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// If subscribe param is not stripped, this will panic with
			// "redis parse ... unexpected option: subscribe"
			client := NewClient(tt.addr, &Config{Retries: 10})
			if client == nil {
				t.Fatalf("Failed to create client - subscribe param may not be properly stripped")
			}

			rm := client.(*rueidisMeta)
			if rm == nil {
				t.Fatal("Expected *rueidisMeta")
			}
		})
	}
}

// TestOPTIN_OPTINModeNilTracking verifies OPTIN mode leaves ClientTrackingOptions nil
// so Rueidis can auto-enable per-key tracking
func TestOPTIN_OPTINModeNilTracking(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"default (optin implicit)", "rueidis://100.121.51.13:6379/7"},
		{"optin explicit", "rueidis://100.121.51.13:6379/7?subscribe=optin"},
		{"optin with ttl", "rueidis://100.121.51.13:6379/7?subscribe=optin&ttl=10s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.addr, &Config{Retries: 10})
			if client == nil {
				t.Fatalf("Failed to create client")
			}

			rm := client.(*rueidisMeta)

			// For OPTIN mode, ClientTrackingOptions MUST be nil or empty
			// so Rueidis automatically sends CLIENT CACHING YES before DoCache()
			if len(rm.option.ClientTrackingOptions) > 0 {
				t.Errorf("OPTIN mode should have nil/empty ClientTrackingOptions, got %v",
					rm.option.ClientTrackingOptions)
			}

			if rm.subscribeMode != "optin" {
				t.Errorf("Expected subscribeMode=optin, got %q", rm.subscribeMode)
			}
		})
	}
}

// TestOPTIN_BCASTModeHasTracking verifies BCAST mode sets ClientTrackingOptions
func TestOPTIN_BCASTModeHasTracking(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"bcast explicit", "rueidis://100.121.51.13:6379/8?subscribe=bcast"},
		{"bcast with ttl", "rueidis://100.121.51.13:6379/8?subscribe=bcast&ttl=5s"},
		{"bcast with prime", "rueidis://100.121.51.13:6379/8?subscribe=bcast&prime=1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.addr, &Config{Retries: 10})
			if client == nil {
				t.Fatalf("Failed to create client")
			}

			rm := client.(*rueidisMeta)

			// For BCAST mode, ClientTrackingOptions MUST be set
			if len(rm.option.ClientTrackingOptions) == 0 {
				t.Error("BCAST mode should have non-empty ClientTrackingOptions")
			}

			if rm.subscribeMode != "bcast" {
				t.Errorf("Expected subscribeMode=bcast, got %q", rm.subscribeMode)
			}

			// Verify BCAST keyword is present
			hasBcast := false
			for _, opt := range rm.option.ClientTrackingOptions {
				if opt == "BCAST" {
					hasBcast = true
					break
				}
			}
			if !hasBcast {
				t.Error("BCAST mode should have BCAST keyword in ClientTrackingOptions")
			}
		})
	}
}

// TestOPTIN_ParameterValidation tests that only valid subscribe values are accepted
func TestOPTIN_ParameterValidation(t *testing.T) {
	tests := []struct {
		name          string
		addr          string
		expectedMode  string
		shouldDefault bool
	}{
		{"empty value", "rueidis://100.121.51.13:6379/9?subscribe=", "optin", true},
		{"invalid string", "rueidis://100.121.51.13:6379/9?subscribe=invalid", "optin", true},
		{"BCAST uppercase", "rueidis://100.121.51.13:6379/9?subscribe=BCAST", "optin", true}, // case-sensitive
		{"OPTIN uppercase", "rueidis://100.121.51.13:6379/9?subscribe=OPTIN", "optin", true}, // case-sensitive
		{"typo: bcast", "rueidis://100.121.51.13:6379/9?subscribe=bcst", "optin", true},
		{"valid optin", "rueidis://100.121.51.13:6379/9?subscribe=optin", "optin", false},
		{"valid bcast", "rueidis://100.121.51.13:6379/9?subscribe=bcast", "bcast", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.addr, &Config{Retries: 10})
			if client == nil {
				t.Fatalf("Failed to create client")
			}

			rm := client.(*rueidisMeta)
			if rm.subscribeMode != tt.expectedMode {
				t.Errorf("Expected subscribeMode=%q, got %q", tt.expectedMode, rm.subscribeMode)
			}
		})
	}
}

// TestOPTIN_BackwardCompatibility tests that existing code still works
// (implicit BCAST behavior is now OPTIN, but can be restored with ?subscribe=bcast)
func TestOPTIN_BackwardCompatibility(t *testing.T) {
	tests := []struct {
		name               string
		addr               string
		shouldHaveTracking bool
	}{
		{
			name:               "legacy: default now uses OPTIN (empty tracking)",
			addr:               "rueidis://100.121.51.13:6379/10",
			shouldHaveTracking: false, // OPTIN = no tracking options
		},
		{
			name:               "restore BCAST: explicit subscribe=bcast",
			addr:               "rueidis://100.121.51.13:6379/10?subscribe=bcast",
			shouldHaveTracking: true, // BCAST = has tracking options
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.addr, &Config{Retries: 10})
			if client == nil {
				t.Fatalf("Failed to create client")
			}

			rm := client.(*rueidisMeta)
			hasTracking := len(rm.option.ClientTrackingOptions) > 0

			if hasTracking != tt.shouldHaveTracking {
				t.Errorf("Expected tracking=%v, got %v", tt.shouldHaveTracking, hasTracking)
			}
		})
	}
}

// TestOPTIN_MultipleClientsIndependentModes tests that multiple clients can
// use different subscription modes independently
func TestOPTIN_MultipleClientsIndependentModes(t *testing.T) {
	// Client 1: OPTIN mode
	addr1 := "rueidis://100.121.51.13:6379/11"
	client1 := NewClient(addr1, &Config{Retries: 10})
	if client1 == nil {
		t.Fatal("Failed to create client1")
	}
	rm1 := client1.(*rueidisMeta)

	// Client 2: BCAST mode
	addr2 := "rueidis://100.121.51.13:6379/12?subscribe=bcast"
	client2 := NewClient(addr2, &Config{Retries: 10})
	if client2 == nil {
		t.Fatal("Failed to create client2")
	}
	rm2 := client2.(*rueidisMeta)

	// Verify they use different modes
	if rm1.subscribeMode != "optin" {
		t.Errorf("Client1: expected optin, got %q", rm1.subscribeMode)
	}
	if rm2.subscribeMode != "bcast" {
		t.Errorf("Client2: expected bcast, got %q", rm2.subscribeMode)
	}

	// Verify tracking options match modes
	if len(rm1.option.ClientTrackingOptions) > 0 {
		t.Error("Client1 (OPTIN) should have empty ClientTrackingOptions")
	}
	if len(rm2.option.ClientTrackingOptions) == 0 {
		t.Error("Client2 (BCAST) should have non-empty ClientTrackingOptions")
	}
}
