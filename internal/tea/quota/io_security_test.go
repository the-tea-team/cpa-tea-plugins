package quota

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSnapshotFileSecurity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	if err := WriteSnapshot(path, baseSnapshot()); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(path); err != nil {
		t.Fatalf("0600: %v", err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(path); err == nil {
		t.Error("accepted group/world-writable snapshot")
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(path); err != nil {
		t.Errorf("0640: %v", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(link); err == nil {
		t.Error("accepted symlink")
	}
}
