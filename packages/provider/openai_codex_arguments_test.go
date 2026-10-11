package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesParallelToolFinalArguments(t *testing.T) {
	for _, tt := range []struct {
		name          string
		deltaBytes    int
		argumentsDone bool
		itemDone      bool
	}{
		{"argument done without deltas", 0, true, false},
		{"item done without deltas", 0, false, true},
		{"partial interleaved deltas", 18, true, true},
		{"complete interleaved deltas", -1, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var wire strings.Builder
			emit := func(value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				wire.WriteString("data: " + string(data) + "\n\n")
			}
			arguments := []string{`{"command":"printf first","timeout":10}`, `{"command":"printf second","timeout":20}`}
			ids := []string{"call_first", "call_second"}
			for index, id := range ids {
				emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"type": "function_call", "call_id": id, "name": "bash"}})
			}
			if tt.deltaBytes != 0 {
				// Finalize in reverse order to exercise independent accumulators.
				for _, index := range []int{1, 0} {
					delta := arguments[index]
					if tt.deltaBytes > 0 {
						delta = delta[:tt.deltaBytes]
					}
					emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "delta": delta})
				}
			}
			for _, index := range []int{1, 0} {
				if tt.argumentsDone {
					emit(map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "arguments": arguments[index]})
				}
				item := map[string]any{"type": "function_call"}
				if tt.itemDone {
					item["arguments"] = arguments[index]
				}
				emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
			}
			emit(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}})

			client := NewOpenAICodex("test-token", "test-account", "").(*codexClient)
			request := Request{Model: "gpt-6.1-sol"}
			body, err := client.buildRequest(request)
			if err != nil || !body.ParallelToolCalls {
				t.Fatalf("parallel calls disabled: %+v, %v", body, err)
			}
			events := make(chan Event, 16)
			go client.runStream(context.Background(), &http.Response{Body: io.NopCloser(strings.NewReader(wire.String()))}, request, events)
			streamed := map[string]string{}
			ends := map[string]int{}
			var done EventDone
			for event := range events {
				switch e := event.(type) {
				case EventToolArgs:
					streamed[e.ID] += e.Delta
				case EventToolEnd:
					ends[e.ID]++
				case EventDone:
					done = e
				}
			}
			if done.Err != nil || done.Stop != StopToolUse || len(done.Message.Content) != len(ids) {
				t.Fatalf("completion = %+v", done)
			}
			for index, id := range ids {
				call, ok := done.Message.Content[index].(ToolCallBlock)
				if !ok || call.ID != id || call.Name != "bash" || string(call.Arguments) != arguments[index] {
					t.Fatalf("call %d = %+v, want %s", index, call, arguments[index])
				}
				if streamed[id] != arguments[index] || ends[id] != 1 {
					t.Fatalf("stream for %s = %q, ends = %d", id, streamed[id], ends[id])
				}
			}
		})
	}
}

func TestResponsesFinalArgumentSnapshots(t *testing.T) {
	const valid = `{"command":"printf final"}`
	for _, tt := range []struct {
		name  string
		delta string
		final any
		want  string
	}{
		{"missing snapshot", valid, nil, valid},
		{"empty snapshot", valid, "", valid},
		{"authoritative snapshot", "{}", valid, valid},
		{"genuine empty object", "", "{}", "{}"},
		{"no arguments supplied", "", nil, "{}"},
		{"invalid snapshot fails closed", valid, `{"command":`, "{}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			delta, _ := json.Marshal(tt.delta)
			final, _ := json.Marshal(tt.final)
			wire := "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_test\",\"name\":\"bash\"}}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":" + string(delta) + "}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"output_index\":0,\"arguments\":" + string(final) + "}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
			events := make(chan Event, 16)
			client := &codexClient{providerName: "openai-responses"}
			go client.runStream(context.Background(), &http.Response{Body: io.NopCloser(strings.NewReader(wire))}, Request{Model: "gpt-6.1-sol"}, events)
			var done EventDone
			for event := range events {
				if result, ok := event.(EventDone); ok {
					done = result
				}
			}
			if done.Err != nil || len(done.Message.Content) != 1 {
				t.Fatalf("completion = %+v", done)
			}
			call := done.Message.Content[0].(ToolCallBlock)
			if string(call.Arguments) != tt.want {
				t.Fatalf("arguments = %s, want %s", call.Arguments, tt.want)
			}
		})
	}
}
