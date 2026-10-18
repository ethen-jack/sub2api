package service

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func observeOutputText(t *testing.T, s *anthropicStreamOutput, text string) {
	t.Helper()
	encoded, err := json.Marshal(text)
	require.NoError(t, err)
	s.observe(`{"type":"content_block_delta","delta":{"type":"text_delta","text":` + string(encoded) + `}}`)
}

func TestAnthropicOutputEstimateIndependentOfSSEFragmentation(t *testing.T) {
	text := "Hello, 世界! Tool calls and thinking also consume output tokens."
	whole, fragments := &anthropicStreamOutput{}, &anthropicStreamOutput{}
	observeOutputText(t, whole, text)
	for _, r := range text {
		observeOutputText(t, fragments, string(r))
	}
	want, err := whole.estimate()
	require.NoError(t, err)
	got, err := fragments.estimate()
	require.NoError(t, err)
	require.Positive(t, got)
	require.Equal(t, want, got)
}

func TestAnthropicOutputEstimateRespectsUpstreamUsage(t *testing.T) {
	for _, usage := range []string{
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
		`{"type":"message_delta","usage":{"output_tokens":12}}`,
	} {
		s := &anthropicStreamOutput{}
		observeOutputText(t, s, "Some generated output.")
		s.observe(usage)
		n, err := s.estimate()
		require.NoError(t, err)
		require.Zero(t, n, "explicit upstream usage, including zero, wins")
	}
	s := &anthropicStreamOutput{}
	s.observe(`{"type":"message_delta","usage":{"output_tokens":1}}`)
	observeOutputText(t, s, "More output after an intermediate usage snapshot.")
	n, err := s.estimate()
	require.NoError(t, err)
	require.Positive(t, n)
}

func TestAnthropicOutputEstimateCountsContentOnly(t *testing.T) {
	s := &anthropicStreamOutput{}
	for _, data := range []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":100}}}`,
		`{"type":"ping"}`,
		`{"type":"content_block_start","content_block":{"type":"tool_use","id":"tool_1","name":"long_tool_name","input":{}}}`,
		`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"not billable text"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"truncated JSON"}`,
	} {
		s.observe(data)
	}
	n, err := s.estimate()
	require.NoError(t, err)
	require.Zero(t, n)
	s.observe(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Think carefully."}}`)
	s.observe(`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Shanghai\"}"}}`)
	require.Equal(t, `Think carefully.{"city":"Shanghai"}`, s.content.String())
	n, err = s.estimate()
	require.NoError(t, err)
	require.Positive(t, n)
}

func TestAnthropicOutputEstimateBufferIsBounded(t *testing.T) {
	s := &anthropicStreamOutput{}
	s.append(strings.Repeat("界", anthropicOutputEstimateMaxBytes))
	s.append("more")
	require.True(t, s.truncated)
	require.LessOrEqual(t, s.content.Len(), anthropicOutputEstimateMaxBytes)
	require.True(t, utf8.ValidString(s.content.String()))
}
