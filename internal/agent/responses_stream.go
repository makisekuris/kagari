package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/openai/openai-go/v3/packages/ssestream"
)

type responsesTransport struct{}

func (responsesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || res == nil || res.Body == nil {
		return res, err
	}
	mediaType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return res, nil
	}

	decoder := ssestream.NewDecoder(res)
	res.Body = &responsesStreamBody{decoder: decoder}
	res.ContentLength = -1
	res.Header.Del("Content-Length")
	return res, nil
}

type responsesStreamBody struct {
	decoder ssestream.Decoder
	buffer  bytes.Buffer
	err     error
}

func (b *responsesStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for b.buffer.Len() == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if !b.decoder.Next() {
			if err := b.decoder.Err(); err != nil {
				b.err = fmt.Errorf("decode Responses event stream: %w", err)
			} else {
				b.err = io.EOF
			}
			return 0, b.err
		}
		events, err := transformResponsesEvent(b.decoder.Event())
		if err != nil {
			b.err = err
			return 0, err
		}
		for _, event := range events {
			writeResponsesEvent(&b.buffer, event)
		}
	}
	return b.buffer.Read(p)
}

func (b *responsesStreamBody) Close() error { return b.decoder.Close() }

func transformResponsesEvent(event ssestream.Event) ([]ssestream.Event, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(event.Data, &fields); err != nil {
		return []ssestream.Event{event}, nil
	}
	var eventType string
	if err := json.Unmarshal(fields["type"], &eventType); err != nil {
		return []ssestream.Event{event}, nil
	}

	switch eventType {
	case "response.output_item.added":
		item, ok := functionCallItem(fields["item"])
		if !ok {
			return []ssestream.Event{event}, nil
		}
		// The SDK appends added.arguments and later deltas; clear this copy, skip those deltas, and inject the completed snapshot once.
		item["arguments"] = json.RawMessage(`""`)
		itemData, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("encode Responses function-call item: %w", err)
		}
		fields["item"] = itemData
		data, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("encode Responses output-item event: %w", err)
		}
		return []ssestream.Event{{Type: event.Type, Data: data}}, nil
	case "response.function_call_arguments.delta":
		return nil, nil
	case "response.output_item.done":
		item, ok := functionCallItem(fields["item"])
		if !ok {
			return []ssestream.Event{event}, nil
		}
		var itemID string
		var arguments string
		if json.Unmarshal(item["id"], &itemID) != nil || json.Unmarshal(item["arguments"], &arguments) != nil {
			return []ssestream.Event{event}, nil
		}
		outputIndex, ok := fields["output_index"]
		if !ok {
			return []ssestream.Event{event}, nil
		}
		data, err := json.Marshal(map[string]any{
			"type":         "response.function_call_arguments.delta",
			"output_index": outputIndex,
			"item_id":      itemID,
			"delta":        arguments,
		})
		if err != nil {
			return nil, fmt.Errorf("encode Responses function-call delta: %w", err)
		}
		return []ssestream.Event{
			{Type: "response.function_call_arguments.delta", Data: data},
			event,
		}, nil
	}
	return []ssestream.Event{event}, nil
}

func functionCallItem(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	item := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, false
	}
	var itemType string
	if json.Unmarshal(item["type"], &itemType) != nil || itemType != "function_call" {
		return nil, false
	}
	return item, true
}

func writeResponsesEvent(dst *bytes.Buffer, event ssestream.Event) {
	if event.Type != "" {
		dst.WriteString("event: ")
		dst.WriteString(event.Type)
		dst.WriteByte('\n')
	}
	data := bytes.TrimSuffix(event.Data, []byte{'\n'})
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		dst.WriteString("data: ")
		dst.Write(line)
		dst.WriteByte('\n')
	}
	dst.WriteByte('\n')
}
