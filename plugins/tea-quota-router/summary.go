package main

import (
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"sort"
	"time"
)

type opportunity struct {
	AccountID         string    `json:"accountId"`
	Label             string    `json:"label"`
	WindowID          string    `json:"windowId"`
	ResetAt           time.Time `json:"resetAt"`
	RemainingFraction float64   `json:"remainingFraction"`
	Reason            string    `json:"reason"`
}

func opportunities(s quota.Snapshot, now time.Time, age time.Duration) []opportunity {
	out := []opportunity{}
	usable := map[string]bool{}
	for _, c := range s.Credentials {
		if !c.Disabled && !c.Unavailable && !c.NextRetryAt.After(now) {
			usable[c.AccountID] = true
		}
	}
	for _, a := range s.Accounts {
		if !usable[a.ID] {
			continue
		}
		if quota.AccountScore(a, "", now, age).Exhausted {
			continue
		}
		for _, w := range a.Windows {
			if len(w.Models) > 0 || w.UsedFraction == nil || w.ObservedAt.IsZero() || w.ObservedAt.After(now) || now.Sub(w.ObservedAt) > age || !w.ResetAt.After(now) || w.ResetAt.Sub(now) > 24*time.Hour || *w.UsedFraction >= .8 {
				continue
			}
			out = append(out, opportunity{a.ID, a.Label, w.ID, w.ResetAt, 1 - *w.UsedFraction, "Observed allowance resets soon; check model-specific limits before shifting work. Not a forecast."})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResetAt.Before(out[j].ResetAt) })
	return out
}
func (p *runtimeState) summaryData(s quota.Snapshot, rows []usageRow, c Config, window string) map[string]any {
	now := time.Now().UTC()
	p.mu.RLock()
	h := p.control.health
	p.mu.RUnlock()
	if h.Status == "" {
		h.Status = "starting"
	}
	if h.Setup == nil {
		h.Setup = []setupItem{}
	}
	unknown := int64(0)
	for i := range rows {
		rows[i].EstimatedCostUSD = estimateCost(rows[i], c.Prices)
		if rows[i].AccountID == "" {
			unknown += rows[i].Requests
		}
	}
	pools := quota.Summarize(s, now, time.Duration(c.MaxAgeSeconds)*time.Second)
	if pools == nil {
		pools = []quota.PoolSummary{}
	}
	return map[string]any{"snapshot": s, "pools": pools, "usage": rows, "routing": map[string]any{"mode": c.SchedulerMode, "strategies": c.ProviderStrategies, "accountOverridesSupported": false, "decisions": p.scheduler.history()}, "health": map[string]any{"collectorStatus": h.Status, "lastRefreshAt": h.LastRefreshAt, "lastError": h.LastError, "droppedUsageRecords": p.dropped.Load(), "writeErrors": p.writeErrors.Load(), "attributionUnknown": unknown, "collectionStartedAt": p.started, "maxAgeSeconds": c.MaxAgeSeconds}, "opportunities": opportunities(s, now, time.Duration(c.MaxAgeSeconds)*time.Second), "setup": h.Setup, "window": window}
}
