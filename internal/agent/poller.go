package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/opentalon/opentalon/pkg/plugin"
)

// Poll fetches the external data for a poll trigger by calling its MCP
// tool through the host, and returns the decoded response for the mapper
// to extract facts from.
//
// The MCP call goes through the host's orchestrator (the same path the
// LLM would use), so credentials/policy apply. The response is decoded
// from StructuredContent when present (the schema-validated JSON), else
// from the human-readable Content; non-JSON content is wrapped as
// {"text": <content>} so callers always get a value to dot-path into.
func Poll(ctx context.Context, host plugin.HostCaller, pc PollConfig) (any, error) {
	res, err := host.RunAction(ctx, pc.Server, pc.Tool, pc.Args)
	if err != nil {
		return nil, fmt.Errorf("poll %s.%s: %w", pc.Server, pc.Tool, err)
	}

	payload := res.StructuredContent
	if payload == "" {
		payload = res.Content
	}
	if payload == "" {
		return map[string]any{}, nil
	}

	var parsed any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		// Non-JSON content — expose it as text rather than failing the poll.
		return map[string]any{"text": payload}, nil
	}
	return parsed, nil
}
