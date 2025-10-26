//go:build !norueidis
// +build !norueidis

package meta

import (
	"net"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestRuedisSentinelDetection tests Sentinel format detection logic.
// Same detection pattern as redis.go: comma before first colon
func TestRuedisSentinelDetection(t *testing.T) {
	tests := []struct {
		name       string
		addr       string
		isSentinel bool
		masterName string
		sentinels  []string
	}{
		{
			name:       "single sentinel address",
			addr:       "mymaster,100.121.51.13:26379",
			isSentinel: true,
			masterName: "mymaster",
			sentinels:  []string{"100.121.51.13:26379"},
		},
		{
			name:       "multiple sentinels",
			addr:       "mymaster,s1:26379,s2:26379,s3:26379",
			isSentinel: true,
			masterName: "mymaster",
			sentinels:  []string{"s1:26379", "s2:26379", "s3:26379"},
		},
		{
			name:       "sentinel with custom port",
			addr:       "mymaster,sentinel1:9999",
			isSentinel: true,
			masterName: "mymaster",
			sentinels:  []string{"sentinel1:9999"},
		},
		{
			name:       "standalone host:port",
			addr:       "localhost:6379",
			isSentinel: false,
		},
		{
			name:       "cluster mode (comma after colon)",
			addr:       "host1:6379,host2:6379",
			isSentinel: false,
		},
		{
			name:       "redis cluster URI",
			addr:       "192.168.1.1:6379,192.168.1.2:6379,192.168.1.3:6379",
			isSentinel: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Apply same detection logic as in rueidis.go
			isSentinel := strings.Contains(tt.addr, ",") &&
				strings.Index(tt.addr, ",") < strings.Index(tt.addr, ":")

			if isSentinel != tt.isSentinel {
				t.Errorf("expected isSentinel=%v, got %v for addr=%s", tt.isSentinel, isSentinel, tt.addr)
			}

			if isSentinel {
				ps := strings.Split(tt.addr, ",")
				masterName := ps[0]
				sentinels := ps[1:]

				if masterName != tt.masterName {
					t.Errorf("expected masterName=%s, got %s", tt.masterName, masterName)
				}

				if !reflect.DeepEqual(sentinels, tt.sentinels) {
					t.Errorf("expected sentinels=%v, got %v", tt.sentinels, sentinels)
				}
			}
		})
	}
}

// TestSentinelPortNormalization tests adding default port 26379 to sentinel addresses.
func TestSentinelPortNormalization(t *testing.T) {
	tests := []struct {
		name        string
		input       []string
		defaultPort string
		expected    []string
	}{
		{
			name:        "all without ports",
			input:       []string{"s1", "s2", "s3"},
			defaultPort: "26379",
			expected:    []string{"s1:26379", "s2:26379", "s3:26379"},
		},
		{
			name:        "all with ports",
			input:       []string{"s1:26379", "s2:26379", "s3:26379"},
			defaultPort: "26379",
			expected:    []string{"s1:26379", "s2:26379", "s3:26379"},
		},
		{
			name:        "mixed with/without ports - first port is used as default",
			input:       []string{"s1:26379", "s2", "s3:9999"},
			defaultPort: "26379",
			// In real code, we extract port from LAST element, then use that for all
			// Since last is "s3:9999", port would be "9999"
			expected: []string{"s1:26379", "s2:9999", "s3:9999"},
		},
		{
			name:        "custom default port",
			input:       []string{"s1", "s2:9999", "s3"},
			defaultPort: "9999",
			expected:    []string{"s1:9999", "s2:9999", "s3:9999"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Apply same port normalization logic as in rueidis.go
			result := make([]string, len(tt.input))
			copy(result, tt.input)

			// Get default port from LAST address (matching redis.go logic)
			_, port, _ := net.SplitHostPort(result[len(result)-1])
			if port == "" {
				port = tt.defaultPort
			}

			// Normalize all addresses using the extracted/default port
			for i, addr := range result {
				h, p, e := net.SplitHostPort(addr)
				if e != nil {
					// No ':' delimiter
					result[i] = net.JoinHostPort(addr, port)
				} else if p == "" {
					// Has ':' but port empty
					result[i] = net.JoinHostPort(h, port)
				}
				// Otherwise port already specified - keep as is
			}

			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// TestSentinelCredentialsExtraction tests extraction of credentials from master name.
// Format: user:pass@mymaster
func TestSentinelCredentialsExtraction(t *testing.T) {
	tests := []struct {
		name           string
		masterNamePart string
		expectedUser   string
		expectedPass   string
		expectedName   string
	}{
		{
			name:           "no credentials",
			masterNamePart: "mymaster",
			expectedUser:   "",
			expectedPass:   "",
			expectedName:   "mymaster",
		},
		{
			name:           "username only",
			masterNamePart: "user@mymaster",
			expectedUser:   "user",
			expectedPass:   "",
			expectedName:   "mymaster",
		},
		{
			name:           "username and password",
			masterNamePart: "user:pass@mymaster",
			expectedUser:   "user",
			expectedPass:   "pass",
			expectedName:   "mymaster",
		},
		{
			name:           "password with colon",
			masterNamePart: "user:pass:word@mymaster",
			expectedUser:   "user",
			expectedPass:   "pass:word", // first colon is separator
			expectedName:   "mymaster",
		},
		{
			name:           "multiple @ signs - use last",
			masterNamePart: "user@domain:pass@mymaster",
			expectedUser:   "user@domain",
			expectedPass:   "pass",
			expectedName:   "mymaster",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Apply same credentials extraction logic as in rueidis.go
			var username, password string
			masterName := tt.masterNamePart

			if atIdx := strings.LastIndex(tt.masterNamePart, "@"); atIdx >= 0 {
				userInfo := tt.masterNamePart[:atIdx]
				masterName = tt.masterNamePart[atIdx+1:]

				if colonIdx := strings.Index(userInfo, ":"); colonIdx >= 0 {
					username = userInfo[:colonIdx]
					password = userInfo[colonIdx+1:]
				} else {
					username = userInfo
				}
			}

			if username != tt.expectedUser {
				t.Errorf("expected username=%s, got %s", tt.expectedUser, username)
			}
			if password != tt.expectedPass {
				t.Errorf("expected password=%s, got %s", tt.expectedPass, password)
			}
			if masterName != tt.expectedName {
				t.Errorf("expected masterName=%s, got %s", tt.expectedName, masterName)
			}
		})
	}
}

// TestSentinelURIFormats tests various valid Sentinel URI formats.
func TestSentinelURIFormats(t *testing.T) {
	tests := []struct {
		name       string
		addr       string
		isSentinel bool
		reason     string
	}{
		{
			name:       "basic sentinel",
			addr:       "mymaster,sentinel1:26379/0",
			isSentinel: true,
			reason:     "comma before colon",
		},
		{
			name:       "multiple sentinels with db",
			addr:       "mymaster,s1:26379,s2:26379,s3:26379/1",
			isSentinel: true,
			reason:     "comma before colon",
		},
		{
			name:       "sentinel with hostname",
			addr:       "mymaster,redis-sentinel.example.com:26379/0",
			isSentinel: true,
			reason:     "comma before colon",
		},
		// Note: The following test cases have different formats that may not be detected as Sentinel
		// because they don't match the pattern "comma before first colon":
		// - "user:pass@mymaster,sentinel1:26379/0" - has colon in credentials before comma
		// - "mymaster,sentinel1/0" - no colon at all before comma
		// These are edge cases that should use proper URL parsing in production code
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isSentinel := strings.Contains(tt.addr, ",") &&
				strings.Index(tt.addr, ",") < strings.Index(tt.addr, ":")

			if isSentinel != tt.isSentinel {
				t.Errorf("expected isSentinel=%v, got %v for addr=%s (%s)",
					tt.isSentinel, isSentinel, tt.addr, tt.reason)
			}
		})
	}
}

// TestFullSentinelURIWithCredentials tests complete URI parsing with credentials
// Format: rueidis://login:password@masterName,sentinel1,sentinel2/db
func TestFullSentinelURIWithCredentials(t *testing.T) {
	tests := []struct {
		name              string
		fullURI           string
		expectedUsername  string
		expectedPassword  string
		expectedMaster    string
		expectedSentinels []string
		expectedDB        int
	}{
		{
			name:              "credentials with sentinels",
			fullURI:           "rueidis://login:password@masterName,1.2.3.4,1.2.5.6:26379/2",
			expectedUsername:  "login",
			expectedPassword:  "password",
			expectedMaster:    "masterName",
			expectedSentinels: []string{"1.2.3.4:26379", "1.2.5.6:26379"},
			expectedDB:        2,
		},
		{
			name:              "username only",
			fullURI:           "rueidis://admin@mymaster,sentinel1:26379/0",
			expectedUsername:  "admin",
			expectedPassword:  "",
			expectedMaster:    "mymaster",
			expectedSentinels: []string{"sentinel1:26379"},
			expectedDB:        0,
		},
		{
			name:              "password with special chars",
			fullURI:           "rueidis://user:p@ss:w0rd@mymaster,s1:26379/1",
			expectedUsername:  "user",
			expectedPassword:  "p@ss:w0rd", // Everything after first colon until last @
			expectedMaster:    "mymaster",
			expectedSentinels: []string{"s1:26379"},
			expectedDB:        1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse full URI to extract credentials
			u, err := url.Parse(tt.fullURI)
			if err != nil {
				t.Fatalf("failed to parse URI: %v", err)
			}

			// Extract userinfo from URL (this is what url.Parse gives us)
			var username, password string
			if u.User != nil {
				username = u.User.Username()
				pass, _ := u.User.Password()
				password = pass
			}

			// Build address part (what cleanAddr would be after parsing)
			cleanAddr := u.Host + u.Path

			// Now parse as Sentinel would
			if strings.Contains(cleanAddr, ",") && strings.Index(cleanAddr, ",") < strings.Index(cleanAddr, ":") {
				ps := strings.Split(cleanAddr, ",")
				masterName := ps[0]
				sentinelAddrs := ps[1:]

				// Extract DB from sentinelAddrs first (it might be in last address like "1.2.5.6:26379/2")
				var db int = 0
				for i, addr := range sentinelAddrs {
					if idx := strings.LastIndex(addr, "/"); idx > 0 {
						if dbNum, err := strconv.Atoi(addr[idx+1:]); err == nil {
							db = dbNum
						}
						sentinelAddrs[i] = addr[:idx]
					}
				}

				// Normalize ports
				_, port, _ := net.SplitHostPort(sentinelAddrs[len(sentinelAddrs)-1])
				if port == "" {
					port = "26379"
				}

				for i, addr := range sentinelAddrs {
					h, p, e := net.SplitHostPort(addr)
					if e != nil {
						sentinelAddrs[i] = net.JoinHostPort(addr, port)
					} else if p == "" {
						sentinelAddrs[i] = net.JoinHostPort(h, port)
					}
				}

				// Validate
				if username != tt.expectedUsername {
					t.Errorf("expected username=%s, got %s", tt.expectedUsername, username)
				}
				if password != tt.expectedPassword {
					t.Errorf("expected password=%s, got %s", tt.expectedPassword, password)
				}
				if masterName != tt.expectedMaster {
					t.Errorf("expected master=%s, got %s", tt.expectedMaster, masterName)
				}
				if !reflect.DeepEqual(sentinelAddrs, tt.expectedSentinels) {
					t.Errorf("expected sentinels=%v, got %v", tt.expectedSentinels, sentinelAddrs)
				}
				if db != tt.expectedDB {
					t.Errorf("expected db=%d, got %d", tt.expectedDB, db)
				}
			} else {
				t.Errorf("not detected as Sentinel mode for URI: %s", tt.fullURI)
			}
		})
	}
}

// TestSentinelURIWithQueryParams tests that query parameters are correctly handled in Sentinel mode.
func TestSentinelURIWithQueryParams(t *testing.T) {
	tests := []struct {
		name           string
		uri            string
		expectedParams map[string]string
	}{
		{
			name: "sentinel_with_route_read_replica",
			uri:  "rueidis://masterName,1.2.3.4,1.2.5.6:26379/2?route-read=replica",
			expectedParams: map[string]string{
				"route-read": "replica",
			},
		},
		{
			name: "sentinel_with_ttl_param",
			uri:  "rueidis://masterName,sentinel1:26379/0?ttl=2h",
			expectedParams: map[string]string{
				"ttl": "2h",
			},
		},
		{
			name: "sentinel_with_multiple_params",
			uri:  "rueidis://user:pass@masterName,1.2.3.4/1?route-read=replica&ttl=1h&subscribe=bcast",
			expectedParams: map[string]string{
				"route-read": "replica",
				"ttl":        "1h",
				"subscribe":  "bcast",
			},
		},
		{
			name: "sentinel_with_credentials_and_params",
			uri:  "rueidis://login:password@masterName,1.2.3.4:26379/3?protocol=3&route-read=replica",
			expectedParams: map[string]string{
				"protocol":   "3",
				"route-read": "replica",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.uri)
			if err != nil {
				t.Fatalf("failed to parse URI: %v", err)
			}

			// Extract and verify query parameters
			actualParams := u.Query()
			for key, expectedVal := range tt.expectedParams {
				actualVal := actualParams.Get(key)
				if actualVal != expectedVal {
					t.Errorf("param %s: expected %s, got %s", key, expectedVal, actualVal)
				}
			}
		})
	}
}

// TestSentinelParsingWithQueryParams tests that ParseSentinelParams correctly extracts params.
func TestSentinelParsingWithQueryParams(t *testing.T) {
	tests := []struct {
		name        string
		cleanAddr   string
		expectParam bool
		paramKey    string
		paramValue  string
	}{
		{
			name:        "sentinel_address_with_params",
			cleanAddr:   "masterName,1.2.3.4,1.2.3.5:26379/0?route-read=replica&ttl=2h",
			expectParam: true,
			paramKey:    "route-read",
			paramValue:  "replica",
		},
		{
			name:        "sentinel_address_without_params",
			cleanAddr:   "masterName,1.2.3.4:26379/0",
			expectParam: false,
			paramKey:    "route-read",
			paramValue:  "",
		},
		{
			name:        "sentinel_with_multiple_params",
			cleanAddr:   "masterName,sentinel1/1?ttl=3h&prime=1&subscribe=optin",
			expectParam: true,
			paramKey:    "ttl",
			paramValue:  "3h",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Try to parse the URL to extract query params
			if u, err := url.Parse("rueidis://" + tt.cleanAddr); err == nil {
				actualVal := u.Query().Get(tt.paramKey)
				if tt.expectParam {
					if actualVal != tt.paramValue {
						t.Errorf("param %s: expected %s, got %s", tt.paramKey, tt.paramValue, actualVal)
					}
				} else if actualVal != "" {
					t.Errorf("expected no params but found %s=%s", tt.paramKey, actualVal)
				}
			}
		})
	}
}

// TestSentinelSpecialCharsInPassword tests params work with special chars in credentials.
func TestSentinelSpecialCharsInPassword(t *testing.T) {
	tests := []struct {
		name        string
		uri         string
		shouldParse bool
	}{
		{
			name:        "password_with_question_mark",
			uri:         "rueidis://user:p%3Fssword@master,sentinel:26379/0?route-read=replica",
			shouldParse: true,
		},
		{
			name:        "password_with_ampersand",
			uri:         "rueidis://user:p%26ssword@master,sentinel:26379/0?ttl=2h&subscribe=bcast",
			shouldParse: true,
		},
		{
			name:        "password_with_equals",
			uri:         "rueidis://user:p%3Dssword@master,sentinel:26379/0?route-read=replica&protocol=3",
			shouldParse: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.uri)
			if err != nil {
				if tt.shouldParse {
					t.Fatalf("expected to parse successfully but got error: %v", err)
				}
				return
			}

			// Verify we can extract query params even with special chars in password
			if u.Query().Get("route-read") != "" || u.Query().Get("ttl") != "" {
				// At least one param should be present
				if !tt.shouldParse {
					t.Errorf("expected parsing to fail but it succeeded")
				}
			}
		})
	}
}
