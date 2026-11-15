package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

func TestApplySnapshotPersistsAndRejectsInvalid(t *testing.T) {
	shutdown()
	dir := t.TempDir()
	p, err := setup([]byte("dataDir: " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	s := quota.Snapshot{SchemaVersion: quota.SchemaVersion, Revision: 9, Accounts: []quota.Account{}, Credentials: []quota.Credential{}, Pools: []quota.Pool{}}
	if err := p.applySnapshot(s); err != nil {
		t.Fatal(err)
	}
	got, err := quota.ReadSnapshot(filepath.Join(dir, "snapshot-v1.json"))
	if err != nil || got.Revision != 9 {
		t.Fatalf("readback: %+v %v", got, err)
	}
	s.SchemaVersion = 999
	if err := p.applySnapshot(s); err == nil {
		t.Fatal("accepted invalid snapshot")
	}
	if p.snapshotCopy().Revision != 9 {
		t.Fatal("invalid snapshot changed state")
	}
}

func TestSetupLoadsPersistedSnapshot(t *testing.T) {
	shutdown()
	dir := t.TempDir()
	s := quota.Snapshot{SchemaVersion: quota.SchemaVersion, Revision: 42, Accounts: []quota.Account{}, Credentials: []quota.Credential{}, Pools: []quota.Pool{}}
	if err := quota.WriteSnapshot(filepath.Join(dir, "snapshot-v1.json"), s); err != nil {
		t.Fatal(err)
	}
	p, err := setup([]byte("dataDir: " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	if p.snapshotCopy().Revision != s.Revision {
		t.Fatal("snapshot not restored")
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshot-v1.json")); err != nil {
		t.Fatal(err)
	}
}
