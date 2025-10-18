//go:build !norueidis && !short
// +build !norueidis,!short

package meta

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// BenchmarkOPTINMode_BasicOperations measures the baseline performance of OPTIN mode
// for simple Set/Get operations on a live Redis server.
func BenchmarkOPTINMode_BasicOperations(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/0?subscribe=optin&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench:optin:basic:%d", i%100)
		value := fmt.Sprintf("value:%d", i)

		// Set operation
		rm.compat.Set(Background(), key, value, 0)
		// Get operation
		rm.compat.Get(Background(), key)
	}

	b.ReportAllocs()
}

// BenchmarkBCASTMode_BasicOperations measures the baseline performance of BCAST mode
// for simple Set/Get operations on a live Redis server.
func BenchmarkBCASTMode_BasicOperations(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/1?subscribe=bcast&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench:bcast:basic:%d", i%100)
		value := fmt.Sprintf("value:%d", i)

		// Set operation
		rm.compat.Set(Background(), key, value, 0)
		// Get operation
		rm.compat.Get(Background(), key)
	}

	b.ReportAllocs()
}

// BenchmarkOPTINMode_CacheHits measures cache hit performance in OPTIN mode
// by repeatedly accessing the same set of keys.
func BenchmarkOPTINMode_CacheHits(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/2?subscribe=optin&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	// Warm up cache with 50 keys
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("bench:optin:cache:%d", i)
		rm.compat.Set(Background(), key, fmt.Sprintf("value:%d", i), 0)
	}

	time.Sleep(100 * time.Millisecond)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Access from cache (should be hits)
		key := fmt.Sprintf("bench:optin:cache:%d", i%50)
		rm.compat.Get(Background(), key)
	}

	b.ReportAllocs()
}

// BenchmarkBCASTMode_CacheHits measures cache hit performance in BCAST mode
// by repeatedly accessing the same set of keys.
func BenchmarkBCASTMode_CacheHits(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/3?subscribe=bcast&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	// Warm up cache with 50 keys
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("bench:bcast:cache:%d", i)
		rm.compat.Set(Background(), key, fmt.Sprintf("value:%d", i), 0)
	}

	time.Sleep(100 * time.Millisecond)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Access from cache (should be hits)
		key := fmt.Sprintf("bench:bcast:cache:%d", i%50)
		rm.compat.Get(Background(), key)
	}

	b.ReportAllocs()
}

// BenchmarkOPTINMode_MultiClientLoad measures throughput with multiple concurrent
// OPTIN mode clients performing read/write operations.
func BenchmarkOPTINMode_MultiClientLoad(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	numClients := 4

	// Create multiple clients in OPTIN mode
	clients := make([]*rueidisMeta, numClients)
	for i := 0; i < numClients; i++ {
		url := fmt.Sprintf("rueidis://%s/4?subscribe=optin&ttl=1h", redisHost)
		m := NewClient(url, &Config{})
		if m == nil {
			b.Fatalf("cannot connect to Redis at %s", redisHost)
		}
		defer m.Shutdown()

		rm, ok := m.(*rueidisMeta)
		if !ok {
			b.Fatalf("expected rueidisMeta")
		}
		clients[i] = rm
	}

	var wg sync.WaitGroup
	operationsPerClient := b.N / numClients

	b.ResetTimer()

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			client := clients[clientID]

			for j := 0; j < operationsPerClient; j++ {
				key := fmt.Sprintf("bench:multi:optin:%d:%d", clientID, j%100)
				value := fmt.Sprintf("client%d:value:%d", clientID, j)

				client.compat.Set(Background(), key, value, 0)
				client.compat.Get(Background(), key)
			}
		}(i)
	}

	wg.Wait()
	b.ReportAllocs()
}

// BenchmarkBCASTMode_MultiClientLoad measures throughput with multiple concurrent
// BCAST mode clients performing read/write operations.
func BenchmarkBCASTMode_MultiClientLoad(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	numClients := 4

	// Create multiple clients in BCAST mode
	clients := make([]*rueidisMeta, numClients)
	for i := 0; i < numClients; i++ {
		url := fmt.Sprintf("rueidis://%s/5?subscribe=bcast&ttl=1h", redisHost)
		m := NewClient(url, &Config{})
		if m == nil {
			b.Fatalf("cannot connect to Redis at %s", redisHost)
		}
		defer m.Shutdown()

		rm, ok := m.(*rueidisMeta)
		if !ok {
			b.Fatalf("expected rueidisMeta")
		}
		clients[i] = rm
	}

	var wg sync.WaitGroup
	operationsPerClient := b.N / numClients

	b.ResetTimer()

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			client := clients[clientID]

			for j := 0; j < operationsPerClient; j++ {
				key := fmt.Sprintf("bench:multi:bcast:%d:%d", clientID, j%100)
				value := fmt.Sprintf("client%d:value:%d", clientID, j)

				client.compat.Set(Background(), key, value, 0)
				client.compat.Get(Background(), key)
			}
		}(i)
	}

	wg.Wait()
	b.ReportAllocs()
}

// BenchmarkOPTINMode_MixedWorkload tests OPTIN mode with mixed read/write pattern
// (80% reads, 20% writes) to simulate realistic usage.
func BenchmarkOPTINMode_MixedWorkload(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/6?subscribe=optin&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	// Warm up with initial keys
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("bench:optin:mixed:%d", i)
		rm.compat.Set(Background(), key, fmt.Sprintf("value:%d", i), 0)
	}

	time.Sleep(100 * time.Millisecond)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		keyIdx := i % 200

		if i%5 < 4 { // 80% reads
			key := fmt.Sprintf("bench:optin:mixed:%d", keyIdx)
			rm.compat.Get(Background(), key)
		} else { // 20% writes
			key := fmt.Sprintf("bench:optin:mixed:%d", keyIdx)
			rm.compat.Set(Background(), key, fmt.Sprintf("updated:%d", i), 0)
		}
	}

	b.ReportAllocs()
}

// BenchmarkBCASTMode_MixedWorkload tests BCAST mode with mixed read/write pattern
// (80% reads, 20% writes) to simulate realistic usage.
func BenchmarkBCASTMode_MixedWorkload(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/7?subscribe=bcast&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	// Warm up with initial keys
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("bench:bcast:mixed:%d", i)
		rm.compat.Set(Background(), key, fmt.Sprintf("value:%d", i), 0)
	}

	time.Sleep(100 * time.Millisecond)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		keyIdx := i % 200

		if i%5 < 4 { // 80% reads
			key := fmt.Sprintf("bench:bcast:mixed:%d", keyIdx)
			rm.compat.Get(Background(), key)
		} else { // 20% writes
			key := fmt.Sprintf("bench:bcast:mixed:%d", keyIdx)
			rm.compat.Set(Background(), key, fmt.Sprintf("updated:%d", i), 0)
		}
	}

	b.ReportAllocs()
}

// BenchmarkOPTINMode_HighKeyCardinality measures performance with a large number
// of distinct keys (high cardinality workload).
func BenchmarkOPTINMode_HighKeyCardinality(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/8?subscribe=optin&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Each iteration uses a unique key (high cardinality)
		key := fmt.Sprintf("bench:optin:card:%d", i)
		value := fmt.Sprintf("value:%d", i)

		rm.compat.Set(Background(), key, value, 0)
		rm.compat.Get(Background(), key)

		// Clean up every 1000 iterations to prevent unlimited key growth
		if i%1000 == 0 && i > 0 {
			rm.compat.FlushDB(Background())
		}
	}

	b.ReportAllocs()
}

// BenchmarkBCASTMode_HighKeyCardinality measures performance with a large number
// of distinct keys (high cardinality workload) in BCAST mode.
func BenchmarkBCASTMode_HighKeyCardinality(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/9?subscribe=bcast&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Each iteration uses a unique key (high cardinality)
		key := fmt.Sprintf("bench:bcast:card:%d", i)
		value := fmt.Sprintf("value:%d", i)

		rm.compat.Set(Background(), key, value, 0)
		rm.compat.Get(Background(), key)

		// Clean up every 1000 iterations to prevent unlimited key growth
		if i%1000 == 0 && i > 0 {
			rm.compat.FlushDB(Background())
		}
	}

	b.ReportAllocs()
}

// BenchmarkClientTrackingOptions_OPTIN verifies the ClientTrackingOptions
// configuration in OPTIN mode during a benchmark.
func BenchmarkClientTrackingOptions_OPTIN(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/10?subscribe=optin&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	// Verify OPTIN configuration
	if rm.subscribeMode != "optin" {
		b.Fatalf("expected OPTIN mode, got %q", rm.subscribeMode)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench:opts:optin:%d", i%100)
		rm.compat.Get(Background(), key)
	}

	b.ReportAllocs()
}

// BenchmarkClientTrackingOptions_BCAST verifies the ClientTrackingOptions
// configuration in BCAST mode during a benchmark.
func BenchmarkClientTrackingOptions_BCAST(b *testing.B) {
	redisHost := "100.121.51.13:6379"
	url := fmt.Sprintf("rueidis://%s/11?subscribe=bcast&ttl=1h", redisHost)

	m := NewClient(url, &Config{})
	if m == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta")
	}

	// Verify BCAST configuration
	if rm.subscribeMode != "bcast" {
		b.Fatalf("expected BCAST mode, got %q", rm.subscribeMode)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench:opts:bcast:%d", i%100)
		rm.compat.Get(Background(), key)
	}

	b.ReportAllocs()
}

// BenchmarkOPTINMode_InvalidationLatency measures the time for cache invalidation
// messages to propagate in OPTIN mode (cross-client scenario).
func BenchmarkOPTINMode_InvalidationLatency(b *testing.B) {
	redisHost := "100.121.51.13:6379"

	urlA := fmt.Sprintf("rueidis://%s/12?subscribe=optin&ttl=1h", redisHost)
	urlB := fmt.Sprintf("rueidis://%s/12?subscribe=optin&ttl=1h", redisHost)

	mA := NewClient(urlA, &Config{})
	if mA == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer mA.Shutdown()

	mB := NewClient(urlB, &Config{})
	if mB == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer mB.Shutdown()

	rmA, ok := mA.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta for client A")
	}

	rmB, ok := mB.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta for client B")
	}

	testKey := "bench:latency:optin"

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		value := fmt.Sprintf("value:%d", i)

		// Client A writes
		rmA.compat.Set(Background(), testKey, value, 0)

		// Small delay to allow invalidation propagation
		time.Sleep(10 * time.Millisecond)

		// Client B reads (should see the update)
		rmB.compat.Get(Background(), testKey)
	}

	b.ReportAllocs()
}

// BenchmarkBCASTMode_InvalidationLatency measures the time for cache invalidation
// messages to propagate in BCAST mode (cross-client scenario).
func BenchmarkBCASTMode_InvalidationLatency(b *testing.B) {
	redisHost := "100.121.51.13:6379"

	urlA := fmt.Sprintf("rueidis://%s/13?subscribe=bcast&ttl=1h", redisHost)
	urlB := fmt.Sprintf("rueidis://%s/13?subscribe=bcast&ttl=1h", redisHost)

	mA := NewClient(urlA, &Config{})
	if mA == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer mA.Shutdown()

	mB := NewClient(urlB, &Config{})
	if mB == nil {
		b.Fatalf("cannot connect to Redis at %s", redisHost)
	}
	defer mB.Shutdown()

	rmA, ok := mA.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta for client A")
	}

	rmB, ok := mB.(*rueidisMeta)
	if !ok {
		b.Fatalf("expected rueidisMeta for client B")
	}

	testKey := "bench:latency:bcast"

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		value := fmt.Sprintf("value:%d", i)

		// Client A writes
		rmA.compat.Set(Background(), testKey, value, 0)

		// Small delay to allow broadcast propagation
		time.Sleep(10 * time.Millisecond)

		// Client B reads (should see the update)
		rmB.compat.Get(Background(), testKey)
	}

	b.ReportAllocs()
}
