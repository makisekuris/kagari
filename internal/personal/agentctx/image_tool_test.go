package agentctx

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"

	"kagari/internal/channel"
	"kagari/internal/config"
	"kagari/internal/modelprovider"
	"kagari/internal/textpage"
)

func TestImageAnalysisToolUsesSelectedImageAndIsolatesCallbacks(t *testing.T) {
	data := tinyPNG(t)
	files := newImageFiles(data, "application/octet-stream")
	var gotInput []*schema.AgenticMessage
	var gotOptions int
	var gotDeadline time.Time
	var gotDeadlineOK bool
	var parentCallbacks int
	parentHandler := callbacks.NewHandlerBuilder().OnStartFn(func(ctx context.Context, _ *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
		parentCallbacks++
		return ctx
	}).Build()
	parentCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
	defer cancel()
	parentDeadline, _ := parentCtx.Deadline()
	ctx := callbacks.InitCallbacks(parentCtx, &callbacks.RunInfo{Name: "parent"}, parentHandler)
	model := &imageModelStub{generate: func(ctx context.Context, input []*schema.AgenticMessage, options ...model.Option) (*schema.AgenticMessage, error) {
		gotInput = input
		gotOptions = len(options)
		gotDeadline, gotDeadlineOK = ctx.Deadline()
		ctx = callbacks.EnsureRunInfo(ctx, "vision", components.ComponentOfAgenticModel)
		ctx = callbacks.OnStart(ctx, &model.AgenticCallbackInput{Messages: input})
		callbacks.OnEnd(ctx, &model.AgenticCallbackOutput{})
		return imageTextResponse("A red mug sits on a wooden table."), nil
	}}
	base, err := ImageAnalysisTool(files, model, map[string]bool{files.meta.ID: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reply := invokeImageTool(t, ctx, base, files.meta.ID, "What is on the table?")
	if reply.Error != "" || reply.Observation != "A red mug sits on a wooden table." {
		t.Fatalf("reply = %+v", reply)
	}
	if parentCallbacks != 0 {
		t.Fatalf("parent callbacks observed image request %d times", parentCallbacks)
	}
	if gotOptions != 0 {
		t.Fatalf("Generate options = %d, want none", gotOptions)
	}
	if !gotDeadlineOK || !gotDeadline.Equal(parentDeadline) {
		t.Fatalf("vision deadline = %v (%t), want parent deadline %v", gotDeadline, gotDeadlineOK, parentDeadline)
	}
	if len(gotInput) != 2 || gotInput[0].Role != schema.AgenticRoleTypeSystem || gotInput[1].Role != schema.AgenticRoleTypeUser {
		t.Fatalf("vision input = %#v, want only system and current user messages", gotInput)
	}
	if len(gotInput[1].ContentBlocks) != 2 || gotInput[1].ContentBlocks[0].UserInputText == nil || gotInput[1].ContentBlocks[1].UserInputImage == nil {
		t.Fatalf("user input blocks = %#v", gotInput[1].ContentBlocks)
	}
	if got := gotInput[1].ContentBlocks[0].UserInputText.Text; got != "Question: What is on the table?" {
		t.Fatalf("question text = %q", got)
	}
	image := gotInput[1].ContentBlocks[1].UserInputImage
	if image.MIMEType != "image/png" || image.Base64Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("image input MIME=%q base64 matches=%t", image.MIMEType, image.Base64Data == base64.StdEncoding.EncodeToString(data))
	}
}

func TestImageAnalysisToolPreflightsSelectionAndImageData(t *testing.T) {
	pngData := tinyPNG(t)
	oversized := newImageFiles(nil, "image/png")
	oversized.meta.Size = channel.MaxFileBytes + 1
	deleted := newImageFiles(pngData, "image/png")
	deleted.meta.Deleted = true
	errored := newImageFiles(pngData, "image/png")
	errored.meta.Error = "download failed"
	stale := newImageFiles(pngData, "image/png")
	stale.meta.Size++
	unsupported := newImageFiles([]byte("ordinary text"), "image/png")

	tests := []struct {
		name    string
		files   *imageFiles
		allowed map[string]bool
		want    string
	}{
		{name: "unselected", files: newImageFiles(pngData, "image/png"), allowed: map[string]bool{}, want: "not part of this task"},
		{name: "deleted", files: deleted, allowed: map[string]bool{deleted.meta.ID: true}, want: "deleted, stale, or unavailable"},
		{name: "errored", files: errored, allowed: map[string]bool{errored.meta.ID: true}, want: "deleted, stale, or unavailable"},
		{name: "stale size", files: stale, allowed: map[string]bool{stale.meta.ID: true}, want: "deleted, stale, or unavailable"},
		{name: "too large", files: oversized, allowed: map[string]bool{oversized.meta.ID: true}, want: "20,000,000-byte limit"},
		{name: "unsupported bytes despite image MIME metadata", files: unsupported, allowed: map[string]bool{unsupported.meta.ID: true}, want: "use PNG, JPEG, or WebP"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			modelCalls := 0
			vision := &imageModelStub{generate: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
				modelCalls++
				return imageTextResponse("unexpected"), nil
			}}
			base, err := ImageAnalysisTool(test.files, vision, test.allowed, nil)
			if err != nil {
				t.Fatal(err)
			}
			reply := invokeImageTool(t, context.Background(), base, test.files.meta.ID, "Describe it")
			if reply.Observation != "" || !strings.Contains(reply.Error, test.want) {
				t.Fatalf("reply = %+v, want error containing %q", reply, test.want)
			}
			if modelCalls != 0 {
				t.Fatalf("vision model called %d times after preflight failure", modelCalls)
			}
		})
	}
}

func TestImageAnalysisToolAcceptsOnlyDetectedImageMIMEs(t *testing.T) {
	jpegData := tinyJPEG(t)
	tests := []struct {
		name string
		data []byte
		mime string
	}{
		{name: "png", data: tinyPNG(t), mime: "image/png"},
		{name: "jpeg", data: jpegData, mime: "image/jpeg"},
		{name: "webp", data: []byte("RIFF\x04\x00\x00\x00WEBPVP8 "), mime: "image/webp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := newImageFiles(test.data, "text/plain")
			_, mimeType, err := readImageAttachment(context.Background(), files, files.meta.ID)
			if err != nil || mimeType != test.mime {
				t.Fatalf("read image MIME=%q err=%v, want %q", mimeType, err, test.mime)
			}
		})
	}
}

func TestImageAnalysisToolRejectsProviderFailuresAndIncompleteResults(t *testing.T) {
	data := tinyPNG(t)
	tests := []struct {
		name  string
		model *imageModelStub
		want  string
	}{
		{name: "provider unavailable", model: &imageModelStub{generate: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
			return nil, errors.New("secret provider response body")
		}}, want: "image analysis is unavailable"},
		{name: "timeout", model: &imageModelStub{generate: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
			return nil, context.DeadlineExceeded
		}}, want: "image analysis timed out"},
		{name: "incomplete status with text", model: &imageModelStub{generate: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
			message := imageTextResponse("partial answer")
			message.ResponseMeta = &schema.AgenticResponseMeta{OpenAIExtension: &openai.ResponseMetaExtension{Status: openai.ResponseStatusIncomplete}}
			return message, nil
		}}, want: "image analysis did not complete"},
		{name: "failed status with text", model: &imageModelStub{generate: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
			message := imageTextResponse("partial answer")
			message.ResponseMeta = &schema.AgenticResponseMeta{OpenAIExtension: &openai.ResponseMetaExtension{Status: openai.ResponseStatusFailed}}
			return message, nil
		}}, want: "image analysis did not complete"},
		{name: "incomplete output item with text", model: &imageModelStub{generate: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
			message := imageTextResponse("partial answer")
			message.ContentBlocks[0].Extra = map[string]any{"openai-item-status": "incomplete"}
			return message, nil
		}}, want: "image analysis did not complete"},
		{name: "refusal with text", model: &imageModelStub{generate: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
			message := imageTextResponse("not a visual observation")
			message.ContentBlocks[0].AssistantGenText.OpenAIExtension = &openai.AssistantGenTextExtension{Refusal: &openai.OutputRefusal{Reason: "refused"}}
			return message, nil
		}}, want: "image analysis did not complete"},
		{name: "empty output", model: &imageModelStub{generate: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
			return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}, nil
		}}, want: "the vision model returned no textual observation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := newImageFiles(data, "image/png")
			base, err := ImageAnalysisTool(files, test.model, map[string]bool{files.meta.ID: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			reply := invokeImageTool(t, context.Background(), base, files.meta.ID, "Describe the image")
			if reply.Observation != "" || reply.Error != test.want {
				t.Fatalf("reply = %+v, want error %q", reply, test.want)
			}
			if strings.Contains(reply.Error, "secret provider response body") {
				t.Fatalf("provider body leaked in tool reply: %+v", reply)
			}
		})
	}
}

func TestImageAnalysisToolPreservesParentCancellation(t *testing.T) {
	files := newImageFiles(tinyPNG(t), "image/png")
	started := make(chan struct{})
	vision := &imageModelStub{generate: func(ctx context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	base, err := ImageAnalysisTool(files, vision, map[string]bool{files.meta.ID: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		reply imageAnalysisReply
		err   error
	}
	done := make(chan result, 1)
	go func() {
		reply, err := callImageTool(ctx, base, files.meta.ID, "Describe the image")
		done <- result{reply: reply, err: err}
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("vision model did not start")
	}
	select {
	case got := <-done:
		if got.err != nil || got.reply.Error != "image analysis was cancelled" {
			t.Fatalf("reply = %+v", got.reply)
		}
	case <-time.After(time.Second):
		t.Fatal("image analysis did not return after parent cancellation")
	}
}

func TestImageAnalysisToolSendsImageOverBothProviderAPIs(t *testing.T) {
	for _, apiType := range []config.ModelAPIType{config.ModelAPIResponses, config.ModelAPIChatCompletions} {
		t.Run(string(apiType), func(t *testing.T) {
			data := tinyPNG(t)
			var request map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/v1/responses"
				if apiType == config.ModelAPIChatCompletions {
					wantPath = "/v1/chat/completions"
				}
				if r.Method != http.MethodPost || r.URL.Path != wantPath {
					t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, wantPath)
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if apiType == config.ModelAPIResponses {
					_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"fixture","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"A red mug is visible."}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"A red mug is visible."},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()

			vision, err := modelprovider.New(context.Background(), config.Model{
				APIType: apiType, BaseURL: server.URL + "/v1", APIKey: "test-key", Name: "fixture-model",
				Timeout: 3 * time.Second, MaxOutputTokens: 300,
			})
			if err != nil {
				t.Fatal(err)
			}
			files := newImageFiles(data, "text/plain")
			base, err := ImageAnalysisTool(files, vision, map[string]bool{files.meta.ID: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			reply := invokeImageTool(t, context.Background(), base, files.meta.ID, "What is visible?")
			if reply.Error != "" || reply.Observation != "A red mug is visible." {
				t.Fatalf("reply = %+v", reply)
			}
			if got := findStrings(request, func(value string) bool { return strings.HasPrefix(value, "data:image/png;base64,") }); len(got) != 1 || got[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data) {
				t.Fatalf("wire image values = %#v", got)
			}
			if got := findStrings(request, func(value string) bool { return strings.Contains(value, "Question: What is visible?") }); len(got) != 1 {
				t.Fatalf("wire question occurrences = %#v", got)
			}
			if tools, ok := request["tools"].([]any); ok && len(tools) != 0 {
				t.Fatalf("unexpected tools in vision request: %#v", tools)
			}
		})
	}
}

func TestImageAnalysisToolRejectsIncompleteResponsesWithHTTP200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"incomplete","model":"fixture","output":[{"type":"message","id":"msg_1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"partial observation"}]}],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	}))
	defer server.Close()
	vision, err := modelprovider.New(context.Background(), config.Model{
		APIType: config.ModelAPIResponses, BaseURL: server.URL + "/v1", APIKey: "test-key", Name: "fixture-model",
		Timeout: time.Second, MaxOutputTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	files := newImageFiles(tinyPNG(t), "image/png")
	base, err := ImageAnalysisTool(files, vision, map[string]bool{files.meta.ID: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reply := invokeImageTool(t, context.Background(), base, files.meta.ID, "Describe it")
	if reply.Observation != "" || reply.Error != "image analysis did not complete" {
		t.Fatalf("incomplete HTTP 200 response was accepted: %+v", reply)
	}
}

func TestImageAnalysisToolDoesNotExposeProviderHTTPBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"secret-provider-response"}}`, http.StatusBadGateway)
	}))
	defer server.Close()
	vision, err := modelprovider.New(context.Background(), config.Model{
		APIType: config.ModelAPIResponses, BaseURL: server.URL + "/v1", APIKey: "test-key", Name: "fixture-model",
		Timeout: time.Second, MaxOutputTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	files := newImageFiles(tinyPNG(t), "image/png")
	base, err := ImageAnalysisTool(files, vision, map[string]bool{files.meta.ID: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reply := invokeImageTool(t, context.Background(), base, files.meta.ID, "Describe it")
	if reply.Error != "image analysis is unavailable" || strings.Contains(reply.Error, "secret-provider-response") {
		t.Fatalf("provider failure reply = %+v", reply)
	}
}

func TestImageAnalysisToolBoundsObservation(t *testing.T) {
	files := newImageFiles(tinyPNG(t), "image/png")
	long := strings.Repeat("v", 6000)
	vision := &imageModelStub{generate: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
		return imageTextResponse(long), nil
	}}
	base, err := ImageAnalysisTool(files, vision, map[string]bool{files.meta.ID: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	largeWindow := textpage.WithMaxBytes(context.Background(), 10_000)
	raw, err := callImageToolRaw(largeWindow, base, files.meta.ID, "Describe it")
	if err != nil {
		t.Fatal(err)
	}
	var full imageAnalysisReply
	if err := json.Unmarshal([]byte(raw), &full); err != nil {
		t.Fatal(err)
	}
	if full.Error != "" || full.Truncated || full.Observation != long || len(raw) > textpage.MaxBytes(largeWindow) {
		t.Fatalf("full observation reply bytes=%d reply=%+v", len(raw), full)
	}

	smallWindow := textpage.WithMaxBytes(context.Background(), 1000)
	raw, err = callImageToolRaw(smallWindow, base, files.meta.ID, "Describe it")
	if err != nil {
		t.Fatal(err)
	}
	var clipped imageAnalysisReply
	if err := json.Unmarshal([]byte(raw), &clipped); err != nil {
		t.Fatal(err)
	}
	if clipped.Error != "" || !clipped.Truncated || clipped.Observation == long || len(raw) > textpage.MaxBytes(smallWindow) || !strings.HasSuffix(clipped.Observation, "…") {
		t.Fatalf("bounded observation reply bytes=%d reply=%+v", len(raw), clipped)
	}
}

func invokeImageTool(t *testing.T, ctx context.Context, base tool.BaseTool, id, question string) imageAnalysisReply {
	t.Helper()
	reply, err := callImageTool(ctx, base, id, question)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func callImageTool(ctx context.Context, base tool.BaseTool, id, question string) (imageAnalysisReply, error) {
	output, err := callImageToolRaw(ctx, base, id, question)
	if err != nil {
		return imageAnalysisReply{}, err
	}
	var reply imageAnalysisReply
	if err := json.Unmarshal([]byte(output), &reply); err != nil {
		return imageAnalysisReply{}, errors.New("decode analyze_image reply: " + err.Error())
	}
	return reply, nil
}

func callImageToolRaw(ctx context.Context, base tool.BaseTool, id, question string) (string, error) {
	encoded, err := json.Marshal(imageAnalysisArgs{ID: id, Question: question})
	if err != nil {
		return "", err
	}
	invokable, ok := base.(tool.InvokableTool)
	if !ok {
		return "", errors.New("analyze_image is not invokable")
	}
	return invokable.InvokableRun(ctx, string(encoded))
}

func newImageFiles(data []byte, declaredMIME string) *imageFiles {
	copyData := append([]byte(nil), data...)
	return &imageFiles{
		meta: channel.Attachment{ID: "image-1", Name: "image.bin", MIME: declaredMIME, Size: int64(len(copyData))},
		data: copyData,
	}
}

type imageFiles struct {
	meta channel.Attachment
	data []byte
}

func (f *imageFiles) SaveAttachment(context.Context, string, string, io.Reader) (channel.Attachment, error) {
	return channel.Attachment{}, errors.New("not implemented")
}

func (f *imageFiles) Attachment(_ context.Context, id string) (channel.Attachment, error) {
	if id != f.meta.ID {
		return channel.Attachment{}, errors.New("not found")
	}
	return f.meta, nil
}

func (f *imageFiles) OpenAttachment(_ context.Context, id string) (io.ReadCloser, error) {
	if id != f.meta.ID {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

type imageModelStub struct {
	generate func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error)
}

func (m *imageModelStub) Generate(ctx context.Context, input []*schema.AgenticMessage, options ...model.Option) (*schema.AgenticMessage, error) {
	return m.generate(ctx, input, options...)
}

func (m *imageModelStub) Stream(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	return nil, errors.New("not implemented")
}

func imageTextResponse(text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.AssistantGenText{Text: text}),
		},
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := jpeg.Encode(&data, img, nil); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func findStrings(value any, predicate func(string) bool) []string {
	var result []string
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case string:
			if predicate(value) {
				result = append(result, value)
			}
		case []any:
			for _, item := range value {
				visit(item)
			}
		case map[string]any:
			for _, item := range value {
				visit(item)
			}
		}
	}
	visit(value)
	return result
}
