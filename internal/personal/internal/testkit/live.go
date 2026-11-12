package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/logging"
)

func LiveEngine(t *testing.T) *agent.Engine {
	t.Helper()
	if os.Getenv("KAGARI_LIVE_EVAL") != "1" {
		t.Skip("opt-in real-model personal evaluation")
	}
	dir := os.Getenv("KAGARI_EVAL_CONFIG_DIR")
	if dir == "" {
		t.Fatal("set KAGARI_EVAL_CONFIG_DIR to a usable local configuration directory")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal("cannot resolve evaluation working directory")
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal("cannot read evaluation configuration directory")
	}
	cfg, loadErr := config.Load("")
	chdirErr := os.Chdir(cwd)
	if loadErr != nil || chdirErr != nil {
		t.Fatal("evaluation configuration is unusable; check local config.yaml and .env (contents withheld)")
	}
	cfg.ProfilePath = filepath.Join(t.TempDir(), "empty-profile.md")
	if err := os.WriteFile(cfg.ProfilePath, nil, 0600); err != nil {
		t.Fatal("cannot create isolated empty profile")
	}
	cfg.Agent.Timeout = 2 * time.Minute
	cfg.Agent.Streaming = false
	t.Logf("live model config max_output_tokens=%d model_timeout=%s", cfg.Model.MaxOutputTokens, cfg.Model.Timeout)
	transport := http.DefaultTransport
	http.DefaultTransport = &livePersonalTransport{next: transport, t: t}
	t.Cleanup(func() { http.DefaultTransport = transport })
	engine, err := agent.New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatalf("cannot construct live evaluation model: %s", logging.ErrorReason(err))
	}
	return engine
}

type livePersonalTransport struct {
	next          http.RoundTripper
	t             *testing.T
	printedSchema bool
}

func (transport *livePersonalTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.GetBody != nil {
		body, err := request.GetBody()
		if err == nil {
			var metadata struct {
				ToolChoice json.RawMessage `json:"tool_choice"`
				Text       struct {
					Format struct {
						Type   string `json:"type"`
						Strict *bool  `json:"strict"`
					} `json:"format"`
				} `json:"text"`
				Tools []struct {
					Name       string `json:"name"`
					Strict     *bool  `json:"strict"`
					Parameters struct {
						Required []string `json:"required"`
					} `json:"parameters"`
				} `json:"tools"`
			}
			err = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&metadata)
			_ = body.Close()
			if err == nil {
				names := make([]string, 0, len(metadata.Tools))
				for _, tool := range metadata.Tools {
					names = append(names, tool.Name)
					strict := "omitted"
					if tool.Strict != nil {
						if *tool.Strict {
							strict = "true"
						} else {
							strict = "false"
						}
					}
					if !transport.printedSchema {
						transport.t.Logf("request tool=%s strict=%s required=%v", tool.Name, strict, tool.Parameters.Required)
					}
				}
				if !transport.printedSchema {
					transport.t.Logf("request tool names=%v", names)
					transport.printedSchema = true
				}
				transport.t.Logf("real HTTP request tools=%d tool_choice=%s text.format.type=%s", len(names), metadata.ToolChoice, metadata.Text.Format.Type)
			}
		}
	}
	response, err := transport.next.RoundTrip(request)
	if err != nil || !strings.Contains(response.Header.Get("Content-Type"), "application/json") {
		return response, err
	}
	raw, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(raw))
	if readErr == nil {
		var metadata struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(raw, &metadata) == nil {
			types := make([]string, 0, len(metadata.Output))
			for _, item := range metadata.Output {
				types = append(types, item.Type)
			}
			transport.t.Logf("real HTTP response output types=%v", types)
			if !strings.Contains(strings.Join(types, ","), "function_call") {
				for _, item := range metadata.Output {
					for _, content := range item.Content {
						if content.Type == "output_text" {
							valid := json.Valid([]byte(content.Text))
							transport.t.Logf("fictional final text JSON valid=%v fenced=%v", valid, strings.HasPrefix(strings.TrimSpace(content.Text), "```"))
							if !valid {
								transport.t.Logf("fictional final raw text=%q", content.Text)
							}
						}
					}
				}
			}
		}
	}
	return response, readErr
}
