package proxy

import (
	"strings"
	"testing"
)

func TestParseSSEStream_AnthropicFormat(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	events, err := parseSSEStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) != 6 {
		t.Fatalf("expected 6 events, got %d", len(events))
	}

	// Verify Event and Data are both populated for Anthropic format.
	if events[0].Event != "message_start" {
		t.Errorf("event[0].Event: expected message_start, got %q", events[0].Event)
	}
	if events[0].Data == "" {
		t.Error("event[0].Data should not be empty")
	}

	// Last event should be message_stop.
	if events[5].Event != "message_stop" {
		t.Errorf("last event should be message_stop, got %q", events[5].Event)
	}
}

func TestParseSSEStream_OpenAIFormat(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	events, err := parseSSEStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// OpenAI format has no event: line, so Event should be empty.
	if events[0].Event != "" {
		t.Errorf("OpenAI events should have empty Event, got %q", events[0].Event)
	}

	// Last event should be [DONE].
	if events[2].Data != "[DONE]" {
		t.Errorf("last event data: expected [DONE], got %q", events[2].Data)
	}
}

func TestParseSSEStream_SkipsPing(t *testing.T) {
	stream := "event: ping\ndata: {}\n\n" +
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	events, err := parseSSEStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}

	// Ping should be skipped.
	for _, e := range events {
		if e.Event == "ping" {
			t.Error("ping event should have been skipped")
		}
	}
	if len(events) != 2 {
		t.Errorf("expected 2 events (no ping), got %d", len(events))
	}
}

func TestParseSSEStream_TerminatesAtMessageStop(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" +
		"event: extra_event\ndata: should_not_appear\n\n"

	events, err := parseSSEStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 2 {
		t.Errorf("should stop at message_stop, got %d events", len(events))
	}
}

func TestParseSSEStream_TerminatesAtDONE(t *testing.T) {
	stream := "data: {\"id\":\"1\"}\n\n" +
		"data: [DONE]\n\n" +
		"data: should_not_appear\n\n"

	events, err := parseSSEStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 2 {
		t.Errorf("should stop at [DONE], got %d events", len(events))
	}
}

func TestParseSSEStream_MultiLineData(t *testing.T) {
	stream := "data: line1\ndata: line2\n\n"

	events, err := parseSSEStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "line1\nline2" {
		t.Errorf("expected multi-line data, got %q", events[0].Data)
	}
}

func TestParseSSEStream_IgnoresComments(t *testing.T) {
	stream := ": this is a comment\ndata: {\"id\":\"1\"}\n\ndata: [DONE]\n\n"

	events, err := parseSSEStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 2 {
		t.Errorf("comments should be ignored, got %d events", len(events))
	}
}

func TestParseSSEStream_EmptyStream(t *testing.T) {
	events, err := parseSSEStream(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty stream, got %d", len(events))
	}
}
