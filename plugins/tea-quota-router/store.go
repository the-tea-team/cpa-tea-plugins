package main

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type usageSample struct {
	At                                         time.Time
	AccountID, AuthID, Provider, Model         string
	Failed                                     bool
	InputTokens, OutputTokens, CacheReadTokens int64
}
type usageRow struct {
	AccountID        string `json:"accountId"`
	AuthID           string `json:"authId"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	Failures         int64  `json:"failures"`
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	CacheReadTokens  int64  `json:"cacheReadTokens"`
	uncachedTokens   int64
	uncachedKnown    bool
	EstimatedCostUSD *float64 `json:"estimatedCostUSD"`
}
type usageStore struct{ db *sql.DB }

func openUsageStore(dir string) (*usageStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	file := filepath.Join(dir, "usage.db")
	info, err := os.Lstat(file)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0) {
		return nil, fmt.Errorf("unsafe usage database")
	}
	fd, err := syscall.Open(file, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), file)
	info, err = f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || stat.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, fmt.Errorf("unsafe usage database")
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", file)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS usage (at INTEGER NOT NULL, account TEXT NOT NULL, auth TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, failed INTEGER NOT NULL, input INTEGER NOT NULL, output INTEGER NOT NULL, cache INTEGER NOT NULL); CREATE INDEX IF NOT EXISTS usage_at ON usage(at)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &usageStore{db}, nil
}
func (s *usageStore) insert(v usageSample) error {
	_, err := s.db.Exec(`INSERT INTO usage VALUES(?,?,?,?,?,?,?,?,?)`, v.At.UnixNano(), v.AccountID, v.AuthID, v.Provider, v.Model, v.Failed, v.InputTokens, v.OutputTokens, v.CacheReadTokens)
	return err
}
func (s *usageStore) query(since time.Time) ([]usageRow, error) {
	rows, err := s.db.Query(`SELECT account,auth,provider,model,COUNT(*),SUM(failed),SUM(input),SUM(output),SUM(cache),SUM(CASE WHEN input > cache THEN input-cache ELSE 0 END) FROM usage WHERE at>=? GROUP BY account,auth,provider,model`, since.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageRow{}
	for rows.Next() {
		var r usageRow
		if err := rows.Scan(&r.AccountID, &r.AuthID, &r.Provider, &r.Model, &r.Requests, &r.Failures, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.uncachedTokens); err != nil {
			return nil, err
		}
		r.uncachedKnown = true
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *usageStore) close() error { return s.db.Close() }

func (s *usageStore) prune(before time.Time) error {
	_, err := s.db.Exec(`DELETE FROM usage WHERE at < ?`, before.UnixNano())
	return err
}
