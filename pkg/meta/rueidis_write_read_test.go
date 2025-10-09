//go:build !norueidis
// +build !norueidis

package meta

import (
	"testing"
	"time"
)

// TestRueidisWriteRead verifies that write and read operations work correctly
func TestRueidisWriteRead(t *testing.T) {
	if testing.Short() {
		t.Skip("Skip write/read test in short mode")
	}

	redisURI := "rueidis://100.121.51.13:6379/15" // Use dedicated test DB
	m := NewClient(redisURI, &Config{})
	rm := m.(*rueidisMeta)

	// Clear database
	if err := rm.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	// Format filesystem
	format := &Format{
		Name:      "write-read-test",
		BlockSize: 4096,
		Capacity:  1 << 30,
	}
	if err := rm.doInit(format, true); err != nil {
		t.Fatalf("doInit failed: %v", err)
	}

	ctx := Background()

	// Create a file
	var inode Ino
	var attr Attr
	if st := m.Create(ctx, 1, "testfile", 0644, 022, 0, &inode, &attr); st != 0 {
		t.Fatalf("Create failed: %v", st)
	}
	t.Logf("Created file inode=%d", inode)

	// Allocate a chunk ID
	var chunkID uint64
	if st := m.NewSlice(ctx, &chunkID); st != 0 {
		t.Fatalf("NewSlice failed: %v", st)
	}
	t.Logf("Allocated chunk ID=%d (expected 1)", chunkID)

	if chunkID != 1 {
		t.Errorf("First chunk should be ID 1, got %d", chunkID)
	}

	// Write some data
	slice := Slice{
		Id:   chunkID,
		Size: 1024,
		Off:  0,
		Len:  1024,
	}

	if st := m.Write(ctx, inode, 0, 0, slice, time.Now()); st != 0 {
		t.Fatalf("Write failed: %v", st)
	}
	t.Logf("Wrote slice: id=%d size=%d", slice.Id, slice.Size)

	// Read back
	var slices []Slice
	if st := m.Read(ctx, inode, 0, &slices); st != 0 {
		t.Fatalf("Read failed: %v", st)
	}

	if len(slices) == 0 {
		t.Fatal("Read returned no slices")
	}

	t.Logf("Read back %d slices", len(slices))
	for i, s := range slices {
		t.Logf("  Slice %d: id=%d size=%d off=%d len=%d",
			i, s.Id, s.Size, s.Off, s.Len)

		if s.Id != chunkID {
			t.Errorf("Slice %d: expected id=%d, got id=%d", i, chunkID, s.Id)
		}
		if s.Len != 1024 {
			t.Errorf("Slice %d: expected len=1024, got len=%d", i, s.Len)
		}
	}

	// Get file attributes
	if st := m.GetAttr(ctx, inode, &attr); st != 0 {
		t.Fatalf("GetAttr failed: %v", st)
	}
	t.Logf("File length=%d (expected 1024)", attr.Length)

	if attr.Length != 1024 {
		t.Errorf("File length should be 1024, got %d", attr.Length)
	}
}
