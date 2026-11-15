package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"path/filepath"
	"strings"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

const (
	maxOverrideBytes       = 1 << 20
	maxOverrideCredentials = 4096
	maxOverridePools       = 512
	maxPoolMembers         = 4096
)

var (
	// errInvalidOverrides marks a request the operator can fix (HTTP 400).
	errInvalidOverrides = errors.New("invalid overrides")
	// errStateCommit marks a failed authoritative write (HTTP 500). Nothing
	// changed in memory or in state-v1.json when it is returned.
	errStateCommit = errors.New("state commit failed")
)

type overrideDocument struct {
	Credentials map[string]AccountOverride `json:"credentials"`
	Pools       []quota.Pool               `json:"pools"`
}

// osReadOverride reads the legacy/derived overrides.json copy. The committed
// state in state-v1.json, not this file, is the source of truth.
func osReadOverride(dir string) ([]byte, error) {
	return readSecureFile(filepath.Join(dir, exportOverridesName), maxOverrideBytes)
}

func cloneOverrideDocument(d overrideDocument) overrideDocument {
	out := overrideDocument{Credentials: make(map[string]AccountOverride, len(d.Credentials)), Pools: make([]quota.Pool, len(d.Pools))}
	for id, o := range d.Credentials {
		if o.CapacityByWindow != nil {
			caps := make(map[string]float64, len(o.CapacityByWindow))
			for k, v := range o.CapacityByWindow {
				caps[k] = v
			}
			o.CapacityByWindow = caps
		}
		out.Credentials[id] = o
	}
	for i, p := range d.Pools {
		p.CredentialIDs = append([]string{}, p.CredentialIDs...)
		out.Pools[i] = p
	}
	return out
}

// parseOverrideDocument strictly decodes one JSON overrides document. Both
// "credentials" and "pools" must be present.
func parseOverrideDocument(raw []byte) (overrideDocument, error) {
	if len(raw) > maxOverrideBytes {
		return overrideDocument{}, errors.New("too large")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var v overrideDocument
	if d.Decode(&v) != nil {
		return overrideDocument{}, errors.New("invalid overrides")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return overrideDocument{}, errors.New("trailing data")
	}
	if v.Credentials == nil || v.Pools == nil {
		return overrideDocument{}, errors.New("credentials and pools required")
	}
	return normalizeOverrideDocument(v)
}

// normalizeOverrideDocument validates field bounds and returns a deep copy
// with non-nil containers. Relational checks (credentials, providers,
// conflicts) happen when the view is materialized.
func normalizeOverrideDocument(v overrideDocument) (overrideDocument, error) {
	if len(v.Credentials) > maxOverrideCredentials || len(v.Pools) > maxOverridePools {
		return overrideDocument{}, errors.New("too many overrides")
	}
	for id, o := range v.Credentials {
		if strings.TrimSpace(id) == "" || len(id) > 512 || len(o.AccountID) > 512 || len(o.Label) > 512 || len(o.Plan) > 256 || len(o.BillingType) > 256 {
			return overrideDocument{}, errors.New("invalid override")
		}
		for k, x := range o.CapacityByWindow {
			if k == "" || len(k) > 512 || math.IsNaN(x) || math.IsInf(x, 0) || x <= 0 {
				return overrideDocument{}, errors.New("invalid capacity")
			}
		}
	}
	ids := map[string]bool{}
	for _, pool := range v.Pools {
		if strings.TrimSpace(pool.ID) == "" || len(pool.ID) > 512 || len(pool.Provider) > 256 || len(pool.Label) > 512 || len(pool.BillingType) > 256 || len(pool.CredentialIDs) > maxPoolMembers || ids[pool.ID] {
			return overrideDocument{}, errors.New("invalid pool")
		}
		ids[pool.ID] = true
	}
	return cloneOverrideDocument(v), nil
}

// saveOverrides replaces the operator overrides with raw. The returned error
// wraps errInvalidOverrides for a bad request and errStateCommit when the
// authoritative write failed; in both cases nothing changed. A degraded
// derived export is NOT an error: the commit succeeded (see
// saveOverridesResult for the export status).
func (p *runtimeState) saveOverrides(raw []byte) error {
	_, err := p.saveOverridesResult(raw)
	return err
}

// saveOverridesResult is saveOverrides plus the commit report.
func (p *runtimeState) saveOverridesResult(raw []byte) (commitResult, error) {
	doc, err := parseOverrideDocument(raw)
	if err != nil {
		return commitResult{}, errors.Join(errInvalidOverrides, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	view, _, err := p.nextViewLocked(p.base, doc, true)
	if err != nil {
		return commitResult{}, err
	}
	if err := p.commitLocked(p.base, doc, view); err != nil {
		return commitResult{}, err
	}
	return p.resultLocked(), nil
}
