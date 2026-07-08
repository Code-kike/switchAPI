// Package probe executes minimal real completions against a provider —
// 恢复探测与端点测速共用（research/08 #16：/models 200 证明不了补全链路，
// 必须发真实最小补全；max_tokens=1 成本可忽略）。流量直连供应商（ADR-0001）。
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

// DefaultTimeout is the per-probe budget (research/08 #16).
const DefaultTimeout = 10 * time.Second

// Run performs one probe and never panics/blocks past the timeout.
func Run(ctx context.Context, target wire.ProbeTarget, timeout time.Duration) wire.ProbeResult {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	res := wire.ProbeResult{ProviderID: target.ProviderID}

	var url string
	var body []byte
	base := strings.TrimRight(target.BaseURL, "/")
	switch target.Protocol {
	case "anthropic":
		url = base + "/v1/messages"
		body, _ = json.Marshal(map[string]any{
			"model": target.Model, "max_tokens": 1,
			"messages": []map[string]string{{"role": "user", "content": "ping"}},
		})
	case "openai":
		url = base + "/responses"
		body, _ = json.Marshal(map[string]any{
			"model": target.Model, "input": "ping",
			// Responses API 的下限是 16；探测成本仍可忽略。
			"max_output_tokens": 16, "stream": false,
		})
	default:
		res.Error = "unknown protocol: " + target.Protocol
		return res
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	if target.Protocol == "anthropic" {
		// 双头齐发，与转发器同一兼容面（research/01 C2）。
		req.Header.Set("X-Api-Key", target.APIKey)
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // 排干便于连接复用
	res.Status = resp.StatusCode
	res.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !res.OK {
		res.Error = resp.Status
	}
	return res
}

// RunAll executes every target sequentially（测速语义：同一网络位置逐个测，
// 避免并发互相挤占带宽影响延迟读数）.
func RunAll(ctx context.Context, targets []wire.ProbeTarget, timeout time.Duration) []wire.ProbeResult {
	out := make([]wire.ProbeResult, 0, len(targets))
	for _, tgt := range targets {
		if ctx.Err() != nil {
			break
		}
		out = append(out, Run(ctx, tgt, timeout))
	}
	return out
}
