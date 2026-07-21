// Command vcr-proxy is a wire-level record/replay proxy for the Anthropic
// Messages API, sitting between the opentalon host container and api.anthropic.com
// so the full-stack E2E can replay a real LLM authoring interaction deterministically.
//
// The host's Anthropic provider POSTs non-streaming to <base_url>/v1/messages
// (see opentalon internal/provider/anthropic.go). Point that base_url at this
// proxy and it serves recorded responses in order — mirroring the host's own
// in-process VCR Player (internal/vcr), which replays interactions sequentially
// and ignores the request body. That sidesteps request-normalization: given the
// same host build and prompt, the sequence and count of Complete calls is stable.
//
// Modes (VCR_MODE):
//   record  forward each call to real Anthropic, capture the response, append to
//           the cassette; write on shutdown. Requires ANTHROPIC_API_KEY.
//   replay  serve cassette responses in order; no API key, no network. 500 on
//           exhaustion or on a cassette-missing start.
//
// The cassette stores only response bodies plus a SHA-256 of each request body
// (for drift diagnostics) — never the API key, never the raw request. On replay
// a request-hash mismatch is logged as a warning but still served, matching the
// host Player's request-agnostic behaviour.
//
// Env:
//   VCR_MODE          record | replay          (default replay)
//   VCR_CASSETTE      cassette path            (default ./cassette.json)
//   ADDR              listen address           (default :8788)
//   ANTHROPIC_BASE    upstream base            (default https://api.anthropic.com)
//   ANTHROPIC_API_KEY real key                 (record mode only)
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const messagesPath = "/v1/messages"

// Interaction is one recorded Complete call: the upstream status + response
// body, tagged with a hash of the request that produced it.
type Interaction struct {
	RequestSHA256 string          `json:"request_sha256"`
	Status        int             `json:"status"`
	Response      json.RawMessage `json:"response"`
}

// Cassette is the on-disk record/replay format.
type Cassette struct {
	RecordedAt   time.Time     `json:"recorded_at"`
	Interactions []Interaction `json:"interactions"`
}

type proxy struct {
	mode     string
	path     string
	upstream string
	apiKey   string

	mu       sync.Mutex
	cassette Cassette
	pos      int // replay cursor
	client   *http.Client
}

func main() {
	p := &proxy{
		mode:     getenv("VCR_MODE", "replay"),
		path:     getenv("VCR_CASSETTE", "./cassette.json"),
		upstream: getenv("ANTHROPIC_BASE", "https://api.anthropic.com"),
		apiKey:   os.Getenv("ANTHROPIC_API_KEY"),
		client:   &http.Client{Timeout: 120 * time.Second},
	}
	addr := getenv("ADDR", ":8788")

	switch p.mode {
	case "record":
		if p.apiKey == "" {
			log.Fatal("vcr-proxy: VCR_MODE=record requires ANTHROPIC_API_KEY")
		}
	case "replay":
		if err := p.load(); err != nil {
			log.Fatalf("vcr-proxy: load cassette: %v", err)
		}
		log.Printf("vcr-proxy: replaying %d interactions from %s", len(p.cassette.Interactions), p.path)
	default:
		log.Fatalf("vcr-proxy: unknown VCR_MODE %q (want record|replay)", p.mode)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(messagesPath, p.handleMessages)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{Addr: addr, Handler: mux}

	// In record mode, persist the cassette on SIGINT/SIGTERM so the local
	// recording run captures everything before the process dies.
	if p.mode == "record" {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sig
			if err := p.save(); err != nil {
				log.Printf("vcr-proxy: save on shutdown: %v", err)
			}
			_ = srv.Shutdown(context.Background())
		}()
	}

	log.Printf("vcr-proxy: mode=%s listening on %s upstream=%s cassette=%s", p.mode, addr, p.upstream, p.path)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("vcr-proxy: serve: %v", err)
	}
}

func (p *proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read request: %v", err), http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256(reqBody)
	reqHash := hex.EncodeToString(sum[:])

	if p.mode == "replay" {
		p.serveReplay(w, reqHash)
		return
	}
	p.serveRecord(w, r, reqBody, reqHash)
}

// serveReplay returns the next recorded interaction, request-agnostic (a hash
// mismatch warns but still serves — mirrors the host's VCR Player).
func (p *proxy) serveReplay(w http.ResponseWriter, reqHash string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.pos >= len(p.cassette.Interactions) {
		log.Printf("vcr-proxy: cassette exhausted after %d interactions", p.pos)
		http.Error(w, "vcr-proxy: cassette exhausted", http.StatusInternalServerError)
		return
	}
	it := p.cassette.Interactions[p.pos]
	if it.RequestSHA256 != reqHash {
		log.Printf("vcr-proxy: WARN request drift at interaction %d: recorded %s got %s",
			p.pos, short(it.RequestSHA256), short(reqHash))
	}
	p.pos++
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(it.Status)
	_, _ = w.Write(it.Response)
}

// serveRecord forwards to real Anthropic, captures the response, and appends it.
func (p *proxy) serveRecord(w http.ResponseWriter, r *http.Request, reqBody []byte, reqHash string) {
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		p.upstream+messagesPath, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, fmt.Sprintf("build upstream request: %v", err), http.StatusBadGateway)
		return
	}
	// Copy the provider's headers (x-api-key, anthropic-version, content-type,
	// any beta flags), then force the real key in case the caller sent a stub.
	upReq.Header = r.Header.Clone()
	upReq.Header.Set("x-api-key", p.apiKey)
	upReq.Header.Del("Host")
	// Let Go's transport negotiate + transparently decode compression, so the
	// stored body is plain JSON we can replay without an encoding header.
	upReq.Header.Del("Accept-Encoding")

	upResp, err := p.client.Do(upReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = upResp.Body.Close() }()

	respBody, err := io.ReadAll(upResp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read upstream: %v", err), http.StatusBadGateway)
		return
	}

	p.mu.Lock()
	p.cassette.Interactions = append(p.cassette.Interactions, Interaction{
		RequestSHA256: reqHash,
		Status:        upResp.StatusCode,
		Response:      json.RawMessage(respBody),
	})
	n := len(p.cassette.Interactions)
	// Persist after every interaction. `go run` doesn't forward SIGTERM to the
	// compiled child, so a shutdown-only flush would lose the cassette in CI;
	// writing incrementally makes the file complete at all times.
	err = p.saveLocked()
	p.mu.Unlock()
	if err != nil {
		log.Printf("vcr-proxy: save after interaction %d: %v", n, err)
	}
	log.Printf("vcr-proxy: recorded interaction %d (status %d, %d bytes)", n, upResp.StatusCode, len(respBody))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(upResp.StatusCode)
	_, _ = w.Write(respBody)
}

func (p *proxy) load() error {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &p.cassette)
}

func (p *proxy) save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveLocked()
}

// saveLocked writes the cassette; the caller must hold p.mu.
func (p *proxy) saveLocked() error {
	p.cassette.RecordedAt = time.Now().UTC()
	data, err := json.MarshalIndent(&p.cassette, "", "  ")
	if err != nil {
		return err
	}
	// Write-rename for atomicity: a crash mid-write can't leave a truncated file.
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return err
	}
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
