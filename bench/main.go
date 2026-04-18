package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── Mock 上游 ──────────────────────────────────────────────

// 启动一个假的 Claude API 上游，返回固定响应
func startMockUpstream(addr string, latency time.Duration, stream bool) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		r.Body.Close()

		if latency > 0 {
			time.Sleep(latency)
		}

		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			flusher, _ := w.(http.Flusher)
			events := []string{
				`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_bench","type":"message","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":0}}}`,
				`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from bench mock."}}`,
				`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
				`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
				`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
			}
			for _, ev := range events {
				fmt.Fprintf(w, "%s\n\n", ev)
				flusher.Flush()
			}
			return
		}

		// 非流式
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":    "msg_bench",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-sonnet-4-6",
			"content": []map[string]any{
				{"type": "text", "text": "Hello from bench mock."},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
			},
		}
		json.NewEncoder(w).Encode(resp)
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	return srv
}

// ── 压测客户端 ─────────────────────────────────────────────

func main() {
	concurrency := flag.Int("c", 50, "并发数")
	total := flag.Int("n", 1000, "总请求数")
	gatewayURL := flag.String("url", "", "网关地址，如 http://localhost:8080/v1/messages")
	apiKey := flag.String("key", "", "分组 API Key")
	mockAddr := flag.String("mock", ":19999", "Mock 上游监听地址")
	mockLatencyMs := flag.Int("latency", 5, "Mock 上游模拟延迟(ms)")
	useStream := flag.Bool("stream", false, "使用流式响应")
	skipMock := flag.Bool("skip-mock", false, "不启动 mock（用已有上游）")
	flag.Parse()

	if *gatewayURL == "" || *apiKey == "" {
		fmt.Println("用法: go run bench/main.go -url http://localhost:8080/v1/messages -key sk-xxx [-c 50] [-n 1000]")
		fmt.Println()
		fmt.Println("步骤:")
		fmt.Println("  1. 启动网关: go run main.go")
		fmt.Println("  2. 在后台创建分组，记下 API Key")
		fmt.Println("  3. 添加上游，API 地址填 http://127.0.0.1:19999 (压测工具自带 mock)")
		fmt.Println("  4. 运行: go run bench/main.go -url http://localhost:8080/v1/messages -key <分组APIKey>")
		fmt.Println()
		fmt.Println("参数:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// 启动 mock 上游
	if !*skipMock {
		fmt.Printf("启动 Mock 上游 %s (延迟 %dms, stream=%v)\n", *mockAddr, *mockLatencyMs, *useStream)
		srv := startMockUpstream(*mockAddr, time.Duration(*mockLatencyMs)*time.Millisecond, *useStream)
		defer srv.Close()
		time.Sleep(100 * time.Millisecond)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 64,
		"stream":     *useStream,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: *concurrency + 10,
			MaxConnsPerHost:     *concurrency + 10,
		},
	}

	var (
		ok      int64
		fail    int64
		latencies []time.Duration
		mu      sync.Mutex
		errCounts sync.Map // error string → *int64
	)

	fmt.Printf("压测: %d 并发, %d 总请求 → %s\n", *concurrency, *total, *gatewayURL)
	fmt.Println(strings.Repeat("─", 50))

	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < *total; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()

			t0 := time.Now()
			req, _ := http.NewRequest("POST", *gatewayURL, bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", *apiKey)

			resp, err := client.Do(req)
			elapsed := time.Since(t0)

			if err != nil {
				atomic.AddInt64(&fail, 1)
				errKey := err.Error()
				// 截断长错误信息
				if len(errKey) > 80 {
					errKey = errKey[:80]
				}
				if v, ok := errCounts.LoadOrStore(errKey, new(int64)); ok {
					atomic.AddInt64(v.(*int64), 1)
				} else {
					atomic.StoreInt64(v.(*int64), 1)
				}
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				atomic.AddInt64(&ok, 1)
				mu.Lock()
				latencies = append(latencies, elapsed)
				mu.Unlock()
			} else {
				atomic.AddInt64(&fail, 1)
				errKey := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 120))
				if v, ok := errCounts.LoadOrStore(errKey, new(int64)); ok {
					atomic.AddInt64(v.(*int64), 1)
				} else {
					atomic.StoreInt64(v.(*int64), 1)
				}
			}
		}()
	}
	wg.Wait()
	totalTime := time.Since(start)

	// 统计
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	fmt.Println(strings.Repeat("─", 50))
	fmt.Printf("耗时:       %v\n", totalTime.Round(time.Millisecond))
	fmt.Printf("成功:       %d\n", ok)
	fmt.Printf("失败:       %d\n", fail)
	fmt.Printf("QPS:        %.1f\n", float64(ok)/totalTime.Seconds())
	if len(latencies) > 0 {
		var sum time.Duration
		for _, d := range latencies {
			sum += d
		}
		p50 := latencies[len(latencies)*50/100]
		p95 := latencies[len(latencies)*95/100]
		p99 := latencies[len(latencies)*99/100]
		fmt.Printf("平均延迟:   %v\n", (sum / time.Duration(len(latencies))).Round(time.Microsecond))
		fmt.Printf("P50:        %v\n", p50.Round(time.Microsecond))
		fmt.Printf("P95:        %v\n", p95.Round(time.Microsecond))
		fmt.Printf("P99:        %v\n", p99.Round(time.Microsecond))
		fmt.Printf("最小:       %v\n", latencies[0].Round(time.Microsecond))
		fmt.Printf("最大:       %v\n", latencies[len(latencies)-1].Round(time.Microsecond))
	}

	// 打印错误分布
	if fail > 0 {
		fmt.Println(strings.Repeat("─", 50))
		fmt.Println("错误分布:")
		errCounts.Range(func(key, value any) bool {
			cnt := atomic.LoadInt64(value.(*int64))
			fmt.Printf("  [%d次] %s\n", cnt, key.(string))
			return true
		})
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
