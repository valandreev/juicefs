package meta

import (
	"testing"
	"time"
)

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

// TestRedisBaseline verifies that go-redis works with our test server
func TestRedisBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	redisURL := "redis://100.121.51.13:6379/13"
	m := NewClient(redisURL, &Config{})
	if m == nil {
		t.Fatal("Cannot connect to Redis test server")
	}
	defer m.Shutdown()

	format := &Format{Name: "redis-baseline", DirStats: true}
	if err := m.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}
	if err := m.Init(format, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	ctx := Background()
	rootAttr := &Attr{}
	if st := m.GetAttr(ctx, RootInode, rootAttr); st != 0 {
		t.Fatalf("Root inode missing after Init: %v", st)
	}
	t.Logf("Redis root inode: mode=%o typ=%d", rootAttr.Mode, rootAttr.Typ)

	var dirIno Ino
	dirAttr := &Attr{}
	if st := m.Mkdir(ctx, RootInode, "testdir", 0755, 0, 0, &dirIno, dirAttr); st != 0 {
		t.Fatalf("Mkdir failed: %v", st)
	}
	t.Logf("Created directory with inode %d", dirIno)
}

// TestRueidisSmoke verifies that the Rueidis driver can connect and perform basic operations
func TestRueidisSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Use the documented test Redis server
	redisURL := "redis://100.121.51.13:6379/1"

	t.Run("NewClient", func(t *testing.T) {
		// Test that we can create a Rueidis client
		rueidisURL := "rueidis://100.121.51.13:6379/1"
		m := NewClient(rueidisURL, &Config{})
		if m == nil {
			t.Fatalf("NewClient(%q) returned nil", rueidisURL)
		}
		defer m.Shutdown()

		if m.Name() != "rueidis" {
			t.Errorf("expected Name()=rueidis, got %q", m.Name())
		}
	})

	t.Run("CompareWithRedis", func(t *testing.T) {
		// Create both Redis and Rueidis clients pointing to same database
		redisClient := NewClient(redisURL, &Config{})
		if redisClient == nil {
			t.Skip("Cannot connect to Redis test server")
		}
		defer redisClient.Shutdown()

		rueidisURL := "rueidis://100.121.51.13:6379/1"
		rueidisClient := NewClient(rueidisURL, &Config{})
		if rueidisClient == nil {
			t.Fatal("Cannot connect via Rueidis")
		}
		defer rueidisClient.Shutdown()

		// Both should report same name pattern
		if redisClient.Name() == "" || rueidisClient.Name() == "" {
			t.Error("Client names should not be empty")
		}
	})

	t.Run("MetadataOperations", func(t *testing.T) {
		// Test that Rueidis can perform actual metadata operations
		// Use database 10 to avoid conflicts
		rueidisURL := "rueidis://100.121.51.13:6379/10"
		m := NewClient(rueidisURL, &Config{})
		if m == nil {
			t.Fatal("Cannot connect via Rueidis")
		}
		defer m.Shutdown()

		// Initialize with test format
		format := &Format{Name: "rueidis-test", DirStats: true}
		if err := m.Reset(); err != nil {
			t.Fatalf("Reset failed: %v", err)
		}
		if err := m.Init(format, false); err != nil {
			t.Fatalf("Init failed: %v", err)
		}

		// Verify root inode exists
		ctx := Background()
		rootAttr := &Attr{}
		if st := m.GetAttr(ctx, RootInode, rootAttr); st != 0 {
			t.Fatalf("Root inode missing after Init: %v", st)
		}
		t.Logf("Root inode: mode=%o typ=%d", rootAttr.Mode, rootAttr.Typ)

		// Test Mkdir
		var dirIno Ino
		dirAttr := &Attr{}
		if st := m.Mkdir(ctx, RootInode, "testdir", 0755, 0, 0, &dirIno, dirAttr); st != 0 {
			t.Fatalf("Mkdir failed: %v", st)
		}
		if dirIno == 0 {
			t.Fatal("Mkdir returned zero inode")
		}

		// Test Create (file)
		var fileIno Ino
		fileAttr := &Attr{}
		if st := m.Create(ctx, dirIno, "testfile.txt", 0644, 0, 0, &fileIno, fileAttr); st != 0 {
			t.Fatalf("Create failed: %v", st)
		}
		if fileIno == 0 {
			t.Fatal("Create returned zero inode")
		}

		// Test Write
		if st := m.Write(ctx, fileIno, 0, 0, Slice{Id: 1, Size: 100, Len: 100}, time.Now()); st != 0 {
			t.Fatalf("Write failed: %v", st)
		}

		// Test Read (GetAttr to verify file exists)
		readAttr := &Attr{}
		if st := m.GetAttr(ctx, fileIno, readAttr); st != 0 {
			t.Fatalf("GetAttr failed: %v", st)
		}
		if readAttr.Length != 100 {
			t.Errorf("expected length 100, got %d", readAttr.Length)
		}

		// Test Unlink
		if st := m.Unlink(ctx, dirIno, "testfile.txt"); st != 0 {
			t.Fatalf("Unlink failed: %v", st)
		}

		// Test Rmdir
		if st := m.Rmdir(ctx, RootInode, "testdir"); st != 0 {
			t.Fatalf("Rmdir failed: %v", st)
		}
	})

	t.Run("RueidisVsRedisOperations", func(t *testing.T) {
		// Verify that identical operations produce identical results
		// in both Redis and Rueidis implementations

		// Create Redis client on database 11 to avoid interference
		redisClient := NewClient("redis://100.121.51.13:6379/11", &Config{})
		if redisClient == nil {
			t.Skip("Cannot connect to Redis test server")
		}
		defer redisClient.Shutdown()

		// Create Rueidis client on database 12
		rueidisClient := NewClient("rueidis://100.121.51.13:6379/12", &Config{})
		if rueidisClient == nil {
			t.Fatal("Cannot connect via Rueidis")
		}
		defer rueidisClient.Shutdown()

		// Initialize both with same format
		format := &Format{Name: "comparison-test", DirStats: true}

		if err := redisClient.Reset(); err != nil {
			t.Fatalf("Redis Reset failed: %v", err)
		}
		if err := redisClient.Init(format, false); err != nil {
			t.Fatalf("Redis Init failed: %v", err)
		}

		if err := rueidisClient.Reset(); err != nil {
			t.Fatalf("Rueidis Reset failed: %v", err)
		}
		if err := rueidisClient.Init(format, false); err != nil {
			t.Fatalf("Rueidis Init failed: %v", err)
		}

		ctx := Background()

		// Verify both root inodes exist
		redisRootAttr, rueidisRootAttr := &Attr{}, &Attr{}
		if st := redisClient.GetAttr(ctx, RootInode, redisRootAttr); st != 0 {
			t.Fatalf("Redis root inode missing: %v", st)
		}
		if st := rueidisClient.GetAttr(ctx, RootInode, rueidisRootAttr); st != 0 {
			t.Fatalf("Rueidis root inode missing: %v", st)
		}

		// Perform identical operations on both
		var redisIno, rueidisIno Ino
		redisAttr, rueidisAttr := &Attr{}, &Attr{}

		if st := redisClient.Mkdir(ctx, RootInode, "dir", 0755, 0, 0, &redisIno, redisAttr); st != 0 {
			t.Fatalf("Redis Mkdir failed: %v", st)
		}
		if st := rueidisClient.Mkdir(ctx, RootInode, "dir", 0755, 0, 0, &rueidisIno, rueidisAttr); st != 0 {
			t.Fatalf("Rueidis Mkdir failed: %v", st)
		}

		// Both should return same inode number (2, first allocated after root)
		if redisIno != rueidisIno {
			t.Errorf("Inode mismatch: redis=%d rueidis=%d", redisIno, rueidisIno)
		}

		// Attributes should be identical (except timestamps which may vary slightly)
		if redisAttr.Mode != rueidisAttr.Mode {
			t.Errorf("Mode mismatch: redis=%o rueidis=%o", redisAttr.Mode, rueidisAttr.Mode)
		}
		if redisAttr.Typ != rueidisAttr.Typ {
			t.Errorf("Type mismatch: redis=%d rueidis=%d", redisAttr.Typ, rueidisAttr.Typ)
		}

		// Cleanup
		redisClient.Rmdir(ctx, RootInode, "dir")
		rueidisClient.Rmdir(ctx, RootInode, "dir")
	})
}
