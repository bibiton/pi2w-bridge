package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TTSClient talks to atomros2-tts (https://github.com/JINHER-INFO/atomros2-tts).
// API expected on the Genio side:
//
//	POST /prepare {id,text}  -> background synth, cached by id
//	POST /play    {id}       -> play cached audio, auto-remove
//	DELETE /cache?id=X       -> manual eviction
//
// Failure mode is tolerant: TTS errors NEVER block order execution.
// If the TTS server is unreachable we just log and continue — the robot
// keeps doing its job, voice prompts are best-effort.
type TTSClient struct {
	baseURL string
	client  *http.Client
}

func NewTTSClient(baseURL string) *TTSClient {
	if baseURL == "" {
		return nil
	}
	return &TTSClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		// Prepare is async on the server; small timeout. Play blocks the
		// duration of the audio + a margin → bumped to 60s.
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Prepare kicks off background synthesis on the TTS server. Returns
// quickly (the synth runs in a goroutine on the server side, cached by id).
// Safe to call concurrently for many ids — the server serializes them.
func (t *TTSClient) Prepare(id, text string) error {
	if t == nil || id == "" || text == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"id": id, "text": text})
	resp, err := t.post("/prepare", body, 10*time.Second)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("/prepare HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// Play asks the TTS server to play the audio cached under id and remove it.
// Blocks until playback finishes. Returns server-reported duration_ms.
func (t *TTSClient) Play(id string) (durationMs int64, err error) {
	if t == nil || id == "" {
		return 0, nil
	}
	body, _ := json.Marshal(map[string]any{"id": id})
	resp, err := t.post("/play", body, 60*time.Second)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("/play HTTP %d: %s", resp.StatusCode, string(b))
	}
	var r struct {
		OK         bool  `json:"ok"`
		DurationMs int64 `json:"duration_ms"`
	}
	dec := json.NewDecoder(resp.Body)
	_ = dec.Decode(&r)
	return r.DurationMs, nil
}

// Evict removes a cached entry without playing it. Used when cancelling.
func (t *TTSClient) Evict(id string) {
	if t == nil || id == "" {
		return
	}
	req, _ := http.NewRequest(http.MethodDelete, t.baseURL+"/cache?id="+id, nil)
	resp, err := t.client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func (t *TTSClient) post(path string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, t.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c := t.client
	if timeout != c.Timeout {
		c = &http.Client{Timeout: timeout}
	}
	return c.Do(req)
}

// PrepareOrderVoices walks an order's nodes/actions and fires off /prepare
// for every playVoice action in parallel (server side serializes synthesis).
// Errors are logged but do not block — the dispatcher will fall back to
// log-only behavior if /play later fails.
func (t *TTSClient) PrepareOrderVoices(order *VDA5050Order) {
	if t == nil || order == nil {
		return
	}
	var wg sync.WaitGroup
	count := 0
	for _, node := range order.Nodes {
		for _, action := range node.Actions {
			if action.ActionType != "playVoice" {
				continue
			}
			text := ""
			for _, p := range action.ActionParameters {
				if p.Key == "text" {
					text = p.Value
					break
				}
			}
			if text == "" {
				continue
			}
			count++
			wg.Add(1)
			go func(id, text string) {
				defer wg.Done()
				if err := t.Prepare(id, text); err != nil {
					log.Printf("[TTS] prepare %s failed: %v", id, err)
				}
			}(action.ActionID, text)
		}
	}
	if count > 0 {
		log.Printf("[TTS] prepared %d playVoice actions for order %s", count, order.OrderID)
	}
	// Don't wait — synthesis continues in background on the TTS server.
	// We do wait for the HTTP /prepare ACKs (very quick, ms range).
	wg.Wait()
}
