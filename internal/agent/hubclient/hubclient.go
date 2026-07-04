// Package hubclient keeps the Agent attached to the Hub: pairing, the
// /api/v1/ws/agent connection with reconnect backoff, config snapshot
// persistence (0600), and routing-table swaps into the forwarder.
// Disconnection never stops forwarding — the last snapshot keeps serving
// (CONTEXT.md: 临时降级 lands fully in M4; M1 guarantees "断 Hub 不断转发").
package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Code-kike/switchAPI/internal/agent/forward"
	"github.com/Code-kike/switchAPI/internal/agent/usagebuf"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/Code-kike/switchAPI/internal/shared/version"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// State is the Agent's persisted identity + last config snapshot.
// Contains secrets (device token, local token, upstream keys inside
// LastPush) — always written 0600 (ADR-0005 LAN-trust model).
type State struct {
	HubURL      string           `json:"hub_url"`
	DeviceID    string           `json:"device_id"`
	DeviceToken string           `json:"device_token"`
	LocalToken  string           `json:"local_token"`
	LastPush    *wire.ConfigPush `json:"last_push,omitempty"`
	SavedAt     int64            `json:"saved_at"`
}

// DefaultStatePath is ~/.switchapi/agent-state.json.
func DefaultStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".switchapi", "agent-state.json"), nil
}

// LoadState reads the state file (nil, os.ErrNotExist when absent).
func LoadState(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("状态文件损坏 %s: %w", path, err)
	}
	return &st, nil
}

// SaveState persists atomically with 0600 (dir 0700).
func SaveState(path string, st *State) error {
	st.SavedAt = time.Now().Unix()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// BuildTable converts a ConfigPush into a forwarder routing table. Invalid
// entries are skipped with a log line rather than failing the whole push.
func BuildTable(push *wire.ConfigPush) *forward.RoutingTable {
	t := &forward.RoutingTable{}
	if push == nil {
		return t
	}
	t.Rev = push.Rev
	for app, route := range push.Apps {
		u, err := url.Parse(route.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			log.Printf("hubclient: app %s 的 base_url 非法，跳过: %q", app, route.BaseURL)
			continue
		}
		up := &forward.Upstream{
			ProviderID: route.ProviderID, Protocol: route.Protocol,
			BaseURL: u, APIKey: route.APIKey, ModelRedirects: route.ModelRedirects,
		}
		switch route.Protocol {
		case "anthropic":
			t.Anthropic = up
		case "openai":
			t.OpenAI = up
		default:
			log.Printf("hubclient: 未知协议 %q（app %s），跳过", route.Protocol, app)
		}
	}
	return t
}

// Pair exchanges a one-time code for a device token and persists the state.
// An existing local_token is kept (CC/Codex configs reference it); one is
// generated on first pairing.
func Pair(hubURL, code, name, statePath string) (*State, error) {
	hubURL = strings.TrimRight(hubURL, "/")
	if name == "" {
		name, _ = os.Hostname()
	}
	body, _ := json.Marshal(map[string]string{"code": code, "name": name, "platform": runtime.GOOS})
	req, err := http.NewRequest("POST", hubURL+"/api/v1/devices/pair", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接 Hub %s: %w", hubURL, err)
	}
	defer resp.Body.Close()
	var out struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
		Error    *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := resp.Status
		if out.Error != nil {
			msg = out.Error.Message
		}
		return nil, errors.New("配对失败：" + msg)
	}

	st, err := LoadState(statePath)
	if err != nil {
		st = &State{}
	}
	if st.LocalToken == "" {
		tok, err := cryptoutil.NewToken()
		if err != nil {
			return nil, err
		}
		st.LocalToken = tok
	}
	st.HubURL, st.DeviceID, st.DeviceToken = hubURL, out.DeviceID, out.Token
	if err := SaveState(statePath, st); err != nil {
		return nil, err
	}
	return st, nil
}

// Client maintains the WS loop.
type Client struct {
	statePath string
	state     *State
	fwd       *forward.Server
	buf       *usagebuf.Queue // optional usage queue; nil → no reporting (M1 behavior)
}

// New builds a client around a loaded state.
func New(statePath string, st *State, fwd *forward.Server) *Client {
	return &Client{statePath: statePath, state: st, fwd: fwd}
}

// UseQueue attaches the usage queue whose batches this client pumps to the Hub
// and whose rows it acks on usage_ack. Call before Run. A nil queue leaves the
// client in pure M1 (config-only) mode.
func (c *Client) UseQueue(buf *usagebuf.Queue) { c.buf = buf }

// Run blocks until ctx is done: swap-from-snapshot immediately, then connect
// / reconnect forever with exponential backoff.
func (c *Client) Run(ctx context.Context) {
	if c.state.LastPush != nil {
		c.fwd.Swap(BuildTable(c.state.LastPush))
		log.Printf("hubclient: 以本地快照恢复路由（rev=%d）", c.state.LastPush.Rev)
	}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		ok := c.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if ok {
			attempt = 0 // 这次连接曾健康工作过，从头退避
		}
		d := Backoff(attempt)
		attempt++
		log.Printf("hubclient: 与 Hub 断开，%v 后重连", d.Truncate(time.Millisecond))
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
	}
}

// connectOnce runs one connection lifecycle; reports whether it got healthy
// (received at least one push) before dying.
func (c *Client) connectOnce(ctx context.Context) (healthy bool) {
	wsURL := httpToWS(c.state.HubURL) + "/api/v1/ws/agent"
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + c.state.DeviceToken}},
	})
	dcancel()
	if err != nil {
		log.Printf("hubclient: 连接失败: %v", err)
		return false
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	// Single writer: coder/websocket permits one concurrent reader (the loop
	// below) + one writer. The writer drains a send channel (hello, usage
	// batches) and a 30s heartbeat ticker; everything outbound goes through it.
	send := make(chan wire.Envelope, 64)
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case env := <-send:
				if err := writeEnv(cctx, conn, env); err != nil {
					cancel() // wake the read loop to exit
					return
				}
			case <-tick.C:
				hb, _ := wire.NewEnvelope(wire.TypeHeartbeat, wire.Heartbeat{SentAt: time.Now().Unix()})
				if err := writeEnv(cctx, conn, hb); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	hello, _ := wire.NewEnvelope(wire.TypeHello, wire.Hello{
		Name: hostname(), Platform: runtime.GOOS, Version: version.Version,
	})
	select {
	case send <- hello:
	case <-cctx.Done():
		return false
	}

	// Usage pump: on (re)connect, return any records handed to an unacked batch
	// to the pending pool, then keep flushing pending batches to the Hub.
	if c.buf != nil {
		c.buf.ResetInflight()
		go c.pumpUsage(cctx, send)
	}

	for {
		var env wire.Envelope
		if err := wsjson.Read(cctx, conn, &env); err != nil {
			return healthy
		}
		switch env.Type {
		case wire.TypeConfigPush:
			var push wire.ConfigPush
			if err := env.Decode(&push); err != nil {
				log.Printf("hubclient: 非法 config_push: %v", err)
				continue
			}
			if c.state.LastPush != nil && push.Rev < c.state.LastPush.Rev {
				log.Printf("hubclient: 忽略过期推送 rev=%d（本地 rev=%d）", push.Rev, c.state.LastPush.Rev)
				continue
			}
			c.fwd.Swap(BuildTable(&push))
			c.state.LastPush = &push
			if err := SaveState(c.statePath, c.state); err != nil {
				log.Printf("hubclient: 快照落盘失败: %v", err)
			}
			healthy = true
			log.Printf("hubclient: 已应用配置推送 rev=%d", push.Rev)
		case wire.TypeUsageAck:
			if c.buf == nil {
				continue
			}
			var ack wire.UsageAck
			if err := env.Decode(&ack); err != nil {
				log.Printf("hubclient: 非法 usage_ack: %v", err)
				continue
			}
			c.buf.Ack(ack.BatchID)
		}
	}
}

// pumpUsage backfills all pending usage on connect, then flushes on a 2s cadence
// (or sooner once a burst of ≥50 records has accumulated). It runs until the
// connection context is canceled. Each batch is marked in-flight by NextBatch
// and only deleted when the Hub's usage_ack arrives (at-least-once).
func (c *Client) pumpUsage(ctx context.Context, send chan<- wire.Envelope) {
	c.flushUsage(ctx, send) // reconnect backfill

	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			n, err := c.buf.PendingCount()
			if err != nil || n == 0 {
				continue
			}
			if n >= 50 || time.Since(last) >= 2*time.Second {
				c.flushUsage(ctx, send)
				last = time.Now()
			}
		}
	}
}

// flushUsage drains every currently-pending record into batches of 100 and
// hands them to the writer. Records already in flight are skipped by NextBatch,
// so this terminates once nothing new remains.
func (c *Client) flushUsage(ctx context.Context, send chan<- wire.Envelope) {
	for {
		batch, ok := c.buf.NextBatch(100)
		if !ok {
			return
		}
		env, err := wire.NewEnvelope(wire.TypeUsageBatch, batch)
		if err != nil {
			log.Printf("hubclient: 用量批封装失败: %v", err)
			return
		}
		select {
		case send <- env:
		case <-ctx.Done():
			return
		}
	}
}

func writeEnv(ctx context.Context, conn *websocket.Conn, env wire.Envelope) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(wctx, conn, env)
}

// Backoff is the reconnect delay for the n-th consecutive failure:
// 1s·2^n capped at 60s, with ±20% jitter.
func Backoff(attempt int) time.Duration {
	base := time.Second << min(attempt, 6) // 1s..64s
	if base > 60*time.Second {
		base = 60 * time.Second
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(base) * jitter)
}

func httpToWS(u string) string {
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return "ws://" + u
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
