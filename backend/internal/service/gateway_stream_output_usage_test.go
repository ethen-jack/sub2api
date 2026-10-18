package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAnthropicStreamUsagePreservesInitialOutputTokens(t *testing.T) {
	svc := &GatewayService{}
	for name, parse := range map[string]func(string, *ClaudeUsage){
		"passthrough": parseSSEUsagePassthrough,
		"standard":    svc.parseSSEUsage,
	} {
		t.Run(name, func(t *testing.T) {
			usage := &ClaudeUsage{}
			parse(`{"type":"message_start","message":{"usage":{"input_tokens":4,"output_tokens":1,"cache_read_input_tokens":20}}}`, usage)
			require.Equal(t, 1, usage.OutputTokens, "an interruption before message_delta must retain upstream output usage")
			parse(`{"type":"message_delta","usage":{"output_tokens":7}}`, usage)
			require.Equal(t, 7, usage.OutputTokens, "output usage is cumulative, not additive")
			require.Equal(t, 4, usage.InputTokens)
			require.Equal(t, 20, usage.CacheReadInputTokens)
			parse(`{"type":"message_delta","usage":{"output_tokens":0}}`, usage)
			require.Zero(t, usage.OutputTokens, "explicit upstream zero must not retain an earlier output count")
		})
	}
}

func TestAnthropicInterruptedForwardPreservesInitialOutputOnlyUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		name := "standard"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"hello"}]}`)), PlatformAnthropic)
			require.NoError(t, err)
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0,\"output_tokens\":1}}}\n\n")),
			}}
			svc := newForwardPartialUsageServiceForTest(upstream)
			account := newAnthropicOAuthAccountForPartialUsageTest()
			if passthrough {
				account = newAnthropicAPIKeyAccountForTest()
			}
			result, err := svc.Forward(context.Background(), c, account, parsed)
			require.ErrorContains(t, err, "missing terminal event")
			require.NotNil(t, result, "observed output alone must keep the partial bill alive")
			require.Equal(t, 1, result.Usage.OutputTokens)
		})
	}
}

func TestAnthropicForwardOutputEstimation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	start := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":1,\"cache_read_input_tokens\":20}}}\n\n"
	text := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"This is generated output that must not be free just because the connection was interrupted.\"}}\n\n"
	for _, passthrough := range []bool{false, true} {
		name := "standard/"
		if passthrough {
			name = "passthrough/"
		}
		for _, tc := range []struct {
			name         string
			tail         string
			readErr      error
			disconnected bool
			disabled     bool
			noContent    bool
			wantEstimate bool
			wantError    bool
			wantOutput   int
		}{
			{name: "EOF", wantEstimate: true, wantError: true},
			{name: "read_error", readErr: io.ErrUnexpectedEOF, wantEstimate: true, wantError: true},
			{name: "client_disconnect_then_EOF", disconnected: true, wantEstimate: true, wantError: true},
			{name: "disabled", disabled: true, wantError: true, wantOutput: 1},
			{name: "no_output_received", noContent: true, wantError: true, wantOutput: 1},
			{name: "final_usage", tail: "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\ndata: {\"type\":\"message_stop\"}\n\n", wantOutput: 7},
			{name: "final_zero", tail: "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
			{name: "client_disconnect_final_usage", disconnected: true, tail: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\ndata: {\"type\":\"message_stop\"}\n\n", wantOutput: 7},
		} {
			t.Run(name+tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				if tc.disconnected {
					c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}
				}
				parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"hello"}]}`)), PlatformAnthropic)
				require.NoError(t, err)
				payload := start
				if !tc.noContent {
					payload += text
				}
				payload += tc.tail
				upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       &streamReadCloser{payload: []byte(payload), err: tc.readErr},
				}}
				svc := newForwardPartialUsageServiceForTest(upstream)
				svc.cfg.Gateway.StreamOutputTokenEstimation = !tc.disabled
				account := newAnthropicOAuthAccountForPartialUsageTest()
				if passthrough {
					account = newAnthropicAPIKeyAccountForTest()
				}
				result, err := svc.Forward(context.Background(), c, account, parsed)
				if tc.wantError {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				require.NotNil(t, result)
				require.Equal(t, tc.wantEstimate, result.Usage.OutputTokensEstimated)
				if tc.wantEstimate {
					require.Greater(t, result.Usage.OutputTokens, 1)
				} else {
					require.Equal(t, tc.wantOutput, result.Usage.OutputTokens)
				}
				require.Equal(t, 4, result.Usage.InputTokens)
				require.Equal(t, 20, result.Usage.CacheReadInputTokens)
				require.Equal(t, tc.disconnected, result.ClientDisconnect)
				require.NotContains(t, rec.Body.String(), "output_tokens_estimated", "local billing metadata must not alter the provider stream")
			})
		}
	}
}

func TestAnthropicInterruptedOutputEstimationAfterIdleTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		name := "standard"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}
			svc := newStreamingResponseTestGatewayService()
			svc.cfg.Gateway.StreamOutputTokenEstimation = true
			svc.cfg.Gateway.StreamDataIntervalTimeout = 1
			reader, writer := io.Pipe()
			defer func() { _ = reader.Close() }()
			defer func() { _ = writer.Close() }()
			written := make(chan struct{})
			go func() {
				defer close(written)
				_, _ = io.WriteString(writer, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Output before an upstream timeout.\"}}\n\n")
			}()
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
			var result *streamingResult
			var err error
			if passthrough {
				result, err = svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "claude-sonnet-4")
			} else {
				result, err = svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "claude-sonnet-4", "claude-sonnet-4", false)
			}
			<-written
			require.ErrorContains(t, err, "timeout")
			require.NotNil(t, result)
			require.True(t, result.clientDisconnect)
			require.True(t, result.usage.OutputTokensEstimated)
			require.Positive(t, result.usage.OutputTokens)
			require.Equal(t, 4, result.usage.InputTokens)
		})
	}
}
