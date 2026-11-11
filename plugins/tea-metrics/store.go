package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const chanCapacity = 8192

var errNoPlugin = errors.New("tea-metrics: plugin not initialized")

type sample struct {
	ts        int64
	model     string
	provider  string
	tps       float64 // decode tokens/s, valid when >0
	ttftMs    int64
	inTok     int64
	cacheRead int64
}

type Store struct {
	db        *sql.DB
	ch        chan pluginapi.UsageRecord
	retention time.Duration
	dataDir   string
	dropped   atomic.Int64
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func openStore(path string, retention time.Duration) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("tea-metrics: open db: %w", err)
	}
	db.SetMaxOpenConns(2)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS events(
  ts INTEGER NOT NULL,
  model TEXT NOT NULL,
  provider TEXT NOT NULL,
  stream INTEGER NOT NULL,
  failed INTEGER NOT NULL,
  status INTEGER NOT NULL DEFAULT 0,
  ttft_ms INTEGER NOT NULL DEFAULT 0,
  lat_ms INTEGER NOT NULL DEFAULT 0,
  out_tok INTEGER NOT NULL DEFAULT 0,
  in_tok INTEGER NOT NULL DEFAULT 0,
  cache_read INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);`); err != nil {
		db.Close()
		return nil, fmt.Errorf("tea-metrics: schema: %w", err)
	}
	return &Store{
		db:        db,
		ch:        make(chan pluginapi.UsageRecord, chanCapacity),
		retention: retention,
		dataDir:   filepath.Dir(path),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}, nil
}

func (s *Store) submit(rec pluginapi.UsageRecord) {
	select {
	case s.ch <- rec:
	default:
		if d := s.dropped.Add(1); d%1000 == 1 {
			log.Printf("tea-metrics: dropped %d usage records (writer behind)", d)
		}
	}
}

func (s *Store) startWriter() {
	go func() {
		defer close(s.done)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		batch := make([]pluginapi.UsageRecord, 0, 500)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := s.insertBatch(batch); err != nil {
				log.Printf("tea-metrics: insert failed: %v", err)
			}
			batch = batch[:0]
		}
		for {
			select {
			case rec := <-s.ch:
				batch = append(batch, rec)
				if len(batch) >= 500 {
					flush()
				}
			case <-tick.C:
				flush()
			case <-s.stop:
				for {
					select {
					case rec := <-s.ch:
						batch = append(batch, rec)
					default:
						flush()
						return
					}
				}
			}
		}
	}()
}

func (s *Store) insertBatch(batch []pluginapi.UsageRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT INTO events(ts,model,provider,stream,failed,status,ttft_ms,lat_ms,out_tok,in_tok,cache_read) VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, r := range batch {
		status := 0
		if r.Failed {
			status = r.Failure.StatusCode
		}
		_, err = st.Exec(
			r.RequestedAt.UnixMilli(), r.Model, r.Provider,
			b2i(r.Stream), b2i(r.Failed), status,
			r.TTFT.Milliseconds(), r.Latency.Milliseconds(),
			r.Detail.OutputTokens+r.Detail.ReasoningTokens,
			r.Detail.InputTokens, r.Detail.CacheReadTokens,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) pruneLoop() {
	prune := func() {
		cut := time.Now().Add(-s.retention).UnixMilli()
		res, err := s.db.Exec(`DELETE FROM events WHERE ts < ?`, cut)
		if err != nil {
			log.Printf("tea-metrics: prune failed: %v", err)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("tea-metrics: pruned %d events older than retention", n)
			if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				log.Printf("tea-metrics: checkpoint: %v", err)
			}
		}
	}
	prune()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			prune()
		case <-s.stop:
			return
		}
	}
}

func (s *Store) close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}
