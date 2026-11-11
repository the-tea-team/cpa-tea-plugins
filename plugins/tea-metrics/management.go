package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const summaryPathSuffix = "/plugins/tea-metrics/summary"

func (p *tea_metricsPlugin) RegisterManagement(_ context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	base := strings.TrimRight(strings.TrimSpace(req.BasePath), "/")
	if base == "" {
		return pluginapi.ManagementRegistrationResponse{}, errors.New("tea-metrics: management registration is missing the host base path")
	}
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{{
			Method:      http.MethodGet,
			Path:        base + summaryPathSuffix,
			Description: "Per-model speed, latency, cache and failure metrics",
			Handler:     summaryHandler{store: p.store},
		}},
		Resources: []pluginapi.ResourceRoute{{
			Path:        "/metrics",
			Menu:        "Gateway health",
			Description: "Model and provider speed, latency, cache hit and failure metrics",
			Handler:     pageHandler{},
		}},
	}, nil
}

type summaryHandler struct{ store *Store }

func (h summaryHandler) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	window := req.Query.Get("window")
	if window == "" {
		window = "24h"
	}
	if _, ok := windows[window]; !ok {
		return jsonResp(http.StatusBadRequest, map[string]string{"error": "window must be 24h, 7d or 30d"})
	}
	summary, err := h.store.summary(window, windows[window])
	if err != nil {
		return jsonResp(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return jsonResp(http.StatusOK, summary)
}

type pageHandler struct{}

func (pageHandler) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if strings.HasSuffix(req.Path, ".json") { // convenience: same-origin page debugging
		return jsonResp(http.StatusOK, map[string]string{"plugin": pluginName, "version": pluginVersion})
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       pageHTML,
	}, nil
}

// handleManagementRequest dispatches a host-forwarded request to the right handler. The host
// only forwards paths that match declared routes; matching defensively keeps working if a host
// forwards a prefix or a legacy menu path.
func (p *tea_metricsPlugin) handleManagementRequest(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if strings.Contains(req.Path, summaryPathSuffix) {
		return summaryHandler{store: p.store}.HandleManagement(ctx, req)
	}
	return pageHandler{}.HandleManagement(ctx, req)
}

func jsonResp(status int, v any) (pluginapi.ManagementResponse, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"error":"marshal failed"}`),
		}, nil
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}, nil
}

type seriesPoint struct {
	B int64   `json:"b"` // bucket start, epoch seconds
	V float64 `json:"v"` // median decode tok/s in bucket
}

type modelSummary struct {
	Model      string        `json:"model"`
	Provider   string        `json:"provider"`
	Reqs       int           `json:"reqs"`
	OKPct      float64       `json:"ok_pct"`
	E429       int           `json:"e429"`
	DecodeP50  float64       `json:"decode_p50"`
	DecodeP10  float64       `json:"decode_p10"`
	TTFTP50Ms  int           `json:"ttft_p50_ms"`
	TTFTP90Ms  int           `json:"ttft_p90_ms"`
	PrefillTPS *float64      `json:"prefill_tps"` // nil when cache hit ~100% (prefill is a rounding error)
	CachePct   float64       `json:"cache_pct"`
	Series     []seriesPoint `json:"series"`
}

type summary struct {
	Window        string         `json:"window"`
	GeneratedAtMs int64          `json:"generated_at_ms"`
	Dropped       int64          `json:"dropped"`
	Models        []modelSummary `json:"models"`
}

// summary aggregates the window: cheap grouped totals plus a per-request sample pass for
// percentiles and hourly series. Sample volume at fleet traffic is ~50k/day; fine to sort in memory.
func (s *Store) summary(window string, spec windowSpec) (*summary, error) {
	since := time.Now().Add(-time.Duration(spec.hours) * time.Hour).UnixMilli()
	bucketMs := int64(spec.bucketMin) * 60_000

	out := &summary{Window: window, GeneratedAtMs: time.Now().UnixMilli(), Dropped: s.dropped.Load()}

	type totals struct {
		provider           string
		reqs, failed, e429 int64
		inTok, cacheRead   int64
	}
	totalsByModel := map[string]*totals{}
	rows, err := s.db.Query(`SELECT model, provider, COUNT(*), SUM(failed),
	  SUM(CASE WHEN status=429 THEN 1 ELSE 0 END), SUM(in_tok), SUM(cache_read)
	  FROM events WHERE ts>=? GROUP BY model, provider`, since)
	if err != nil {
		return nil, fmt.Errorf("totals query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var model, provider string
		var t totals
		if err := rows.Scan(&model, &provider, &t.reqs, &t.failed, &t.e429, &t.inTok, &t.cacheRead); err != nil {
			return nil, err
		}
		t.provider = provider
		totalsByModel[model] = &t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	samples := map[string][]sample{}
	srows, err := s.db.Query(`SELECT model, ts, ttft_ms, lat_ms, out_tok, in_tok, cache_read
	  FROM events WHERE ts>=? AND failed=0 AND ttft_ms>0 AND lat_ms>ttft_ms AND out_tok>0`, since)
	if err != nil {
		return nil, fmt.Errorf("sample query: %w", err)
	}
	defer srows.Close()
	for srows.Next() {
		var model string
		var sc sample
		var lat, out, inT, cr int64
		if err := srows.Scan(&model, &sc.ts, &sc.ttftMs, &lat, &out, &inT, &cr); err != nil {
			return nil, err
		}
		decodeMs := lat - sc.ttftMs
		sc.tps = float64(out) * 1000 / float64(decodeMs)
		sc.inTok, sc.cacheRead = inT, cr
		samples[model] = append(samples[model], sc)
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}

	for model, t := range totalsByModel {
		ms := modelSummary{Model: model, Provider: t.provider, Reqs: int(t.reqs), E429: int(t.e429)}
		if t.reqs > 0 {
			ms.OKPct = round1(float64(t.reqs-t.failed) / float64(t.reqs) * 100)
		}
		if t.inTok+t.cacheRead > 0 {
			// provider semantics differ: when cache_read fits inside input tokens the provider
			// counts cached input inside it (openai/meta style); when it exceeds input the
			// provider reports uncached input separately (claude style).
			if t.cacheRead <= t.inTok {
				ms.CachePct = round1(float64(t.cacheRead) / float64(t.inTok) * 100)
			} else {
				ms.CachePct = round1(float64(t.cacheRead) / float64(t.inTok+t.cacheRead) * 100)
			}
		}
		if ss := samples[model]; len(ss) > 0 {
			tps := make([]float64, len(ss))
			ttf := make([]float64, len(ss))
			var prefill []float64
			for i, s := range ss {
				tps[i] = s.tps
				ttf[i] = float64(s.ttftMs)
				uncached := s.inTok - s.cacheRead
				if uncached > 0 && s.ttftMs > 0 {
					prefill = append(prefill, float64(uncached)*1000/float64(s.ttftMs))
				}
			}
			sort.Float64s(tps)
			sort.Float64s(ttf)
			ms.DecodeP50 = round1(percentile(tps, 0.5))
			ms.DecodeP10 = round1(percentile(tps, 0.1))
			ms.TTFTP50Ms = roundInt(percentile(ttf, 0.5))
			ms.TTFTP90Ms = roundInt(percentile(ttf, 0.9))
			if len(prefill) >= len(ss)/4 && ms.CachePct < 99 {
				sort.Float64s(prefill)
				v := round1(percentile(prefill, 0.5))
				ms.PrefillTPS = &v
			}
			ms.Series = bucketMedians(ss, time.Now().Add(-time.Duration(spec.hours)*time.Hour).UnixMilli(), bucketMs)
		}
		out.Models = append(out.Models, ms)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Reqs > out.Models[j].Reqs })
	return out, nil
}
