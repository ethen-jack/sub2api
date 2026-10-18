package service

import (
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

func (s *GatewayService) streamOutputTracker() *anthropicStreamOutput {
	if s.cfg == nil || !s.cfg.Gateway.StreamOutputTokenEstimation {
		return nil
	}
	return &anthropicStreamOutput{}
}

func (s *anthropicStreamOutput) finalize(usage *ClaudeUsage, c *gin.Context, account *Account) {
	if s == nil || usage == nil {
		return
	}
	n, err := s.estimate()
	if err != nil {
		slog.Warn("anthropic_stream_output_estimation_failed", "account_id", account.ID, "error", err)
		return
	}
	if n <= usage.OutputTokens {
		return
	}
	observed := usage.OutputTokens
	usage.OutputTokens = n
	usage.OutputTokensEstimated = true
	slog.Warn("anthropic_stream_output_estimated",
		"billing_request_id", resolveUsageBillingRequestID(c.Request.Context(), c.Writer.Header().Get("x-request-id")),
		"account_id", account.ID, "upstream_output_tokens", observed,
		"estimated_output_tokens", n, "observed_bytes", s.content.Len(),
		"truncated", s.truncated, "tokenizer", "cl100k_base",
		"upstream_request_id", c.Writer.Header().Get("x-request-id"))
}

// Keep a bounded, request-local copy of generated content only. Never retain the
// prompt, SSE envelopes, signatures, or arbitrary JSON in the estimation buffer.
const anthropicOutputEstimateMaxBytes = 1 << 20

type anthropicStreamOutput struct {
	content           strings.Builder
	reportedAfterText bool
	finalOutputUsage  bool
	truncated         bool
}

func (s *anthropicStreamOutput) observe(data string) {
	if s == nil || !gjson.Valid(data) {
		return
	}
	event := gjson.Parse(data)
	switch event.Get("type").String() {
	case "message_delta":
		if v := event.Get("usage.output_tokens"); v.Type == gjson.Number && v.Int() >= 0 {
			s.reportedAfterText = true
			if event.Get("delta.stop_reason").String() != "" {
				s.finalOutputUsage = true
			}
		}
	case "message_stop":
		// Some compatible providers omit stop_reason on message_delta.
		s.finalOutputUsage = s.finalOutputUsage || s.reportedAfterText
	case "content_block_start":
		block := event.Get("content_block")
		switch block.Get("type").String() {
		case "text":
			s.append(block.Get("text").String())
		case "thinking":
			s.append(block.Get("thinking").String())
		case "tool_use", "server_tool_use":
			// An empty input object is a placeholder for input_json_delta, not
			// generated output. Non-empty initial arguments are actual content.
			input := block.Get("input")
			if input.IsObject() && len(input.Map()) > 0 {
				s.append(input.Raw)
			}
		}
	case "content_block_delta":
		delta := event.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			s.append(delta.Get("text").String())
		case "thinking_delta":
			s.append(delta.Get("thinking").String())
		case "input_json_delta":
			s.append(delta.Get("partial_json").String())
		}
	}
}

func (s *anthropicStreamOutput) append(text string) {
	if text == "" {
		return
	}
	s.reportedAfterText = false
	remaining := anthropicOutputEstimateMaxBytes - s.content.Len()
	if len(text) > remaining {
		s.truncated = true
		text = text[:remaining]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	_, _ = s.content.WriteString(text)
}

// estimate returns a local approximation, never authoritative provider usage.
// Count the combined content, not individual SSE chunks: a token can span any
// number of network fragments. cl100k_base is explicitly an approximation for
// Anthropic's private tokenizer; unseen output cannot be reconstructed here.
func (s *anthropicStreamOutput) estimate() (int, error) {
	if s.content.Len() == 0 || s.finalOutputUsage || s.reportedAfterText {
		return 0, nil
	}
	codec, err := tokenizer.Get(tokenizer.Cl100kBase)
	if err != nil {
		return 0, err
	}
	return codec.Count(s.content.String())
}
