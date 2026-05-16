package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ollamaGenerate sends a prompt to Ollama and streams the response token-by-token to stdout.
// Returns the full accumulated response string.
func ollamaGenerate(prompt string) (string, error) {
	cfg := loadConfig()
	return ollamaStream(cfg.DefaultModel, prompt, 180*time.Second)
}

func ollamaStream(model, prompt string, timeout time.Duration) (string, error) {
	type reqBody struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	body, _ := json.Marshal(reqBody{Model: model, Prompt: prompt, Stream: true})

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST",
		"http://localhost:11434/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	// Show a loading indicator until the first token arrives.
	// Model loading can take 10-30s before streaming starts.
	firstToken := make(chan struct{})
	var spinOnce sync.Once
	go func() {
		ticker := time.NewTicker(6 * time.Second)
		defer ticker.Stop()
		secs := 0
		for {
			select {
			case <-firstToken:
				return
			case <-ticker.C:
				secs += 6
				fmt.Printf("\r[MERA] Loading... %ds  ", secs)
			}
		}
	}()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		spinOnce.Do(func() { close(firstToken) })
		return "", fmt.Errorf("ollama unreachable: %w", err)
	}
	defer resp.Body.Close()

	type chunk struct {
		Response string `json:"response"`
		Done     bool   `json:"done"`
		Error    string `json:"error"`
	}

	var (
		full      strings.Builder
		gotFirst  bool
	)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 64*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c chunk
		if err := json.Unmarshal(line, &c); err != nil {
			continue
		}
		if c.Error != "" {
			spinOnce.Do(func() { close(firstToken) })
			return "", fmt.Errorf("ollama: %s", c.Error)
		}
		if c.Response != "" {
			if !gotFirst {
				// Clear loading line, start AI output.
				spinOnce.Do(func() { close(firstToken) })
				fmt.Print("\r[AI]  ")
				gotFirst = true
			}
			fmt.Print(c.Response)
			full.WriteString(c.Response)
		}
		if c.Done {
			if gotFirst {
				fmt.Println()
			}
			break
		}
	}

	spinOnce.Do(func() { close(firstToken) })
	return strings.TrimSpace(full.String()), nil
}

// ollamaCall is a non-streaming fallback used for short internal queries
// where printing to stdout would interfere with structured parsing.
func ollamaCall(model, prompt string, timeout time.Duration) (string, error) {
	type reqBody struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	body, _ := json.Marshal(reqBody{Model: model, Prompt: prompt, Stream: false})

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST",
		"http://localhost:11434/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		secs := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				secs += 8
				fmt.Printf("[MERA] Thinking... %ds\n", secs)
			}
		}
	}()

	resp, err := http.DefaultClient.Do(req)
	close(stop)
	if err != nil {
		return "", fmt.Errorf("ollama unreachable: %w", err)
	}
	defer resp.Body.Close()

	type respBody struct {
		Response string `json:"response"`
		Error    string `json:"error"`
	}
	var r respBody
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("ollama decode: %w", err)
	}
	if r.Error != "" {
		return "", fmt.Errorf("ollama: %s", r.Error)
	}
	return strings.TrimSpace(r.Response), nil
}

func ensureOllama() error {
	client := http.Client{Timeout: 2 * time.Second}
	if _, e := client.Get("http://localhost:11434/api/tags"); e == nil {
		return nil
	}
	fmt.Println("[WARN] Ollama not reachable. Attempting auto-start...")
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd.exe", "/c", "ollama serve")
	} else {
		cmd = exec.Command("ollama", "serve")
	}
	_ = cmd.Start()
	time.Sleep(8 * time.Second)
	_, e := client.Get("http://localhost:11434/api/tags")
	return e
}
