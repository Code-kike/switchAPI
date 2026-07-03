package main

// main.go — runnable demo: fake upstream on 127.0.0.1:19528, proxy on
// 127.0.0.1:19527 (as specified by research item #06), then a client request
// through the proxy printing per-event arrival gaps and the parsed usage.

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	upstream := newUpstream(50*time.Millisecond, 8)
	upSrv := &http.Server{
		Addr:              "127.0.0.1:19528",
		Handler:           upstream,
		ReadHeaderTimeout: 5 * time.Second,
		// WriteTimeout MUST stay 0 for SSE: it would cut long streams.
	}
	go func() { log.Fatal(upSrv.ListenAndServe()) }()

	// REAL_UPSTREAM mode: point the same forwarder at a real relay for the M0
	// acceptance run (implement.md M0: one streaming conversation through the
	// prototype against a real provider). Defaults preserve the fake-upstream demo.
	upBase := "http://127.0.0.1:19528"
	upKey := "sk-prototype-upstream-key"
	model := "claude-3-5-haiku-20241022"
	realMode := false
	if v := os.Getenv("REAL_UPSTREAM"); v != "" {
		upBase, upKey, realMode = v, os.Getenv("REAL_KEY"), true
		bearerAuth = os.Getenv("REAL_AUTH") == "bearer"
		if m := os.Getenv("REAL_MODEL"); m != "" {
			model = m
		}
	}
	target, err := url.Parse(upBase)
	if err != nil {
		log.Fatal(err)
	}
	usageCh := make(chan Usage, 1)
	proxy := newProxy(target, upKey, map[string]string{
		"claude-3-5-haiku-20241022": "claude-haiku-4-5",
	}, func(u Usage) { usageCh <- u })

	pxSrv := &http.Server{
		Addr:              "127.0.0.1:19527",
		Handler:           proxy,
		ReadHeaderTimeout: 5 * time.Second,
		// WriteTimeout deliberately 0 (SSE); IdleTimeout only affects keep-alive idling.
		IdleTimeout: 120 * time.Second,
	}
	go func() { log.Fatal(pxSrv.ListenAndServe()) }()
	waitReady("127.0.0.1:19527")
	waitReady("127.0.0.1:19528")

	body := `{"model": "` + model + `", "stream": true, "max_tokens": 64, "messages": [{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "http://127.0.0.1:19527/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer local-agent-token")

	tr := &http.Transport{DisableCompression: true}
	start := time.Now()
	resp, err := (&http.Client{Transport: tr}).Do(req) // NOTE: no Client.Timeout — it would kill the stream
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	fmt.Printf("status=%d content-type=%s\n", resp.StatusCode, resp.Header.Get("Content-Type"))

	// Read the raw stream, timestamping each complete SSE event.
	br := bufio.NewReader(resp.Body)
	var events int
	var buf bytes.Buffer
	last := time.Now()
	for {
		line, err := br.ReadString('\n')
		buf.WriteString(line)
		if line == "\n" { // blank line terminates an SSE event
			events++
			now := time.Now()
			first := strings.TrimSpace(strings.SplitN(buf.String(), "\n", 2)[0])
			fmt.Printf("event %2d  +%6.1fms  gap %5.1fms  %s\n",
				events, float64(now.Sub(start).Microseconds())/1000,
				float64(now.Sub(last).Microseconds())/1000, first)
			last = now
			buf.Reset()
		}
		if err != nil {
			break
		}
	}

	if !realMode {
		rb, cl, te, auth, _ := upstream.snapshot()
		fmt.Printf("\nupstream saw: Content-Length=%d TransferEncoding=%v X-Api-Key=…%s\n",
			cl, te, auth[max(0, len(auth)-4):])
		fmt.Printf("upstream body: %s\n", rb)
	}
	select {
	case u := <-usageCh:
		fmt.Printf("parsed usage: %+v\n", u)
	case <-time.After(time.Second):
		fmt.Println("no usage parsed (BUG)")
		os.Exit(1)
	}
}

func waitReady(addr string) {
	for i := 0; i < 100; i++ {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	log.Fatalf("server %s never became ready", addr)
}
