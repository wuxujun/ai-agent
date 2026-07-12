package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	_ "github.com/wuxujun/ai-agent/internal/multiagent"
)

type evalCase struct {
	Name             string `json:"name"`
	Scene            string `json:"scene"`
	SystemPrompt     string `json:"system_prompt"`
	UserPrompt       string `json:"user_prompt"`
	ExpectedContains string `json:"expected_contains"`
}

func main() {
	input := flag.String("input", "Sample/llm-eval.jsonl", "JSONL evaluation cases")
	flag.Parse()
	_ = config.Get()
	file, err := os.Open(*input)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []string{"answer"}}
	total, passed, tokens := 0, 0, 0
	var elapsed time.Duration
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var item evalCase
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			panic(err)
		}
		var output struct {
			Answer string `json:"answer"`
		}
		start := time.Now()
		usage, callErr := llmcore.CallJSON(context.Background(), llmcore.ConfigForScene(item.Scene), item.SystemPrompt, item.UserPrompt, schema, &output)
		duration := time.Since(start)
		ok := callErr == nil && strings.Contains(strings.ToLower(output.Answer), strings.ToLower(item.ExpectedContains))
		fmt.Printf("name=%q scene=%s pass=%t latency=%s tokens=%d error=%v\n", item.Name, item.Scene, ok, duration, usage.TotalTokens, callErr)
		total++
		if ok {
			passed++
		}
		tokens += usage.TotalTokens
		elapsed += duration
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
	fmt.Printf("summary cases=%d passed=%d pass_rate=%.2f total_tokens=%d total_latency=%s\n", total, passed, float64(passed)/float64(max(total, 1)), tokens, elapsed)
}
