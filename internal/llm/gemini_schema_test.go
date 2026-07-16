package llm

import (
	"testing"

	"google.golang.org/genai"
)

func TestGeminiSchemaFromJSONPreservesFinalizerConstraints(t *testing.T) {
	schema, err := geminiSchemaFromJSON(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"final_answer": map[string]any{"type": "string", "minLength": 1},
			"confidence":   map[string]any{"type": "string", "enum": []string{"high", "low"}},
		},
		"required": []string{"final_answer", "confidence"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if schema.Type != genai.TypeObject || len(schema.Required) != 2 {
		t.Fatalf("root schema = %+v", schema)
	}
	answer := schema.Properties["final_answer"]
	if answer == nil || answer.Type != genai.TypeString || answer.MinLength == nil || *answer.MinLength != 1 {
		t.Fatalf("final_answer schema = %+v", answer)
	}
	confidence := schema.Properties["confidence"]
	if confidence == nil || len(confidence.Enum) != 2 || confidence.Enum[0] != "high" {
		t.Fatalf("confidence schema = %+v", confidence)
	}
}

func TestGeminiSchemaFromJSONPreservesNestedArrayConstraints(t *testing.T) {
	schema, err := geminiSchemaFromJSON(map[string]any{
		"type":     "array",
		"minItems": 1,
		"maxItems": 3,
		"items": map[string]any{
			"type":       "object",
			"properties": map[string]any{"id": map[string]any{"type": "integer"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if schema.Type != genai.TypeArray || schema.MinItems == nil || *schema.MinItems != 1 || schema.MaxItems == nil || *schema.MaxItems != 3 {
		t.Fatalf("array schema = %+v", schema)
	}
	if schema.Items == nil || schema.Items.Properties["id"].Type != genai.TypeInteger {
		t.Fatalf("items schema = %+v", schema.Items)
	}
}

func TestGeminiSchemaFromJSONDoesNotInventTypeForAnyOf(t *testing.T) {
	schema, err := geminiSchemaFromJSON(map[string]any{
		"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "integer"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if schema.Type != "" || len(schema.AnyOf) != 2 {
		t.Fatalf("anyOf schema = %+v", schema)
	}
}
