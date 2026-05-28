package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/wuxujun/ai-agent/pkg/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var tracer = otel.Tracer("agent-runtime/planner")

type LLMPlanner struct {
	APIKey  string
	Model   string
	BaseURL string
	Client  *http.Client
}

func NewLLMPlanner(apiKey, model, baseURL string) *LLMPlanner {
	return &LLMPlanner{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: baseURL,
		Client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (p *LLMPlanner) PlanNext(ctx context.Context, task *types.Task) (*PlanDecision, error) {
	ctx, span := tracer.Start(ctx, "planner.plan_next")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("llm.model", p.Model),
		attribute.Int("agent.task.step_count", task.StepCount),
	)

	systemPrompt := BuildSystemPrompt()
	userPrompt := BuildUserPrompt(task)

	reqBody := map[string]any{
		"model": p.Model,
		"input": []map[string]any{
			{
				"role": "system",
				"content": []map[string]any{
					{"type": "input_text", "text": systemPrompt},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": userPrompt},
				},
			},
		},
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   "planner_decision",
				"strict": true,
				"schema": PlannerDecisionSchema(),
			},
		},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal failed")
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL, bytes.NewReader(b))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "request creation failed")
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.Client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner request failed")
		return nil, err
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode >= 300 {
		err := fmt.Errorf("planner API returned status %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner API error")
		return nil, err
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decode failed")
		return nil, err
	}

	textValue, err := extractStructuredText(raw)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "extract failed")
		return nil, err
	}

	var decision PlanDecision
	if err := json.Unmarshal([]byte(textValue), &decision); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decision unmarshal failed")
		return nil, fmt.Errorf("invalid planner decision: %w", err)
	}

	if err := ValidateDecision(&decision); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decision validation failed")
		return nil, err
	}

	span.SetAttributes(
		attribute.String("agent.planner.action", decision.Action),
		attribute.Bool("agent.planner.stop", decision.Stop),
	)

	return &decision, nil
}

func extractStructuredText(raw map[string]any) (string, error) {
	output, ok := raw["output"].([]any)
	if !ok || len(output) == 0 {
		return "", errors.New("missing output")
	}

	for _, item := range output {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content, ok := obj["content"].([]any)
		if !ok {
			continue
		}
		for _, c := range content {
			part, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if txt, ok := part["text"].(string); ok && txt != "" {
				return txt, nil
			}
		}
	}

	return "", errors.New("structured text not found")
}
