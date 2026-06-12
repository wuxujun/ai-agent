package planner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/tools"
)

// Validator is the optional contract a tool can implement to participate in
// planner-side parameter validation. ValidateDecision dispatches to Validate
// whenever the underlying tool implements it; tools without Validate are
// passed through (the tool's own Execute remains the authoritative gate).
//
// Implementing this interface keeps validation in lock-step with the tool
// registry: registering a new tool with a Validate method makes its parameter
// checks immediately effective, no edits to validate.go required. This closes
// the previous gap where git_diff / http_fetch / web_search / use_skill all
// fell through validate.go's hardcoded switch and only failed at execute time.
type Validator interface {
	Validate(params map[string]any) error
}

func ValidateDecision(d *PlanDecision) error {
	if len(d.Actions) == 0 {
		return errors.New("decision must contain at least one action")
	}

	for _, ac := range d.Actions {
		// "none" is the sentinel stop action and is never a registered tool.
		// Every other action must correspond to a tool in the registry, so the
		// set of valid actions stays in lock-step with PlannerDecisionSchema
		// (both derive from tools.DefaultRegistry).
		if ac.Action == "none" {
			if !d.Stop {
				return errors.New("action=none requires stop=true")
			}
			if strings.TrimSpace(d.FinalAnswer) == "" {
				return errors.New("stop decision requires final_answer")
			}
			continue
		}

		if d.Stop {
			return errors.New("stop=true requires action=none")
		}

		tool, ok := tools.Get(ac.Action)
		if !ok {
			return fmt.Errorf("invalid action: %s", ac.Action)
		}

		// Tools that implement the optional Validator interface get their
		// per-tool checks invoked here. Tools that don't are passed through
		// (Execute remains the final gate). The middleware wrapper does not
		// satisfy Validator itself, so we have to unwrap it to the underlying
		// tool — registry.Register wraps every tool in toolMiddleware.
		if v, ok := unwrapValidator(tool); ok {
			if err := v.Validate(ac.Parameters); err != nil {
				return fmt.Errorf("validation failed for action %s: %w", ac.Action, err)
			}
		}
	}
	return nil
}

// unwrapValidator inspects tool for a Validator. Because the default registry
// wraps every tool in an internal middleware, we accept either:
//  1. tool itself implementing Validator (rare — bare tool registered manually)
//  2. tool exposing an Unwrap() tools.Tool whose result implements Validator
//     (the standard registry path)
//
// The Unwrap escape hatch is added to tools.toolMiddleware in the same change
// to keep the wrapper transparent for validation.
func unwrapValidator(tool tools.Tool) (Validator, bool) {
	if v, ok := tool.(Validator); ok {
		return v, true
	}
	type unwrapper interface{ Unwrap() tools.Tool }
	if u, ok := tool.(unwrapper); ok {
		if v, ok := u.Unwrap().(Validator); ok {
			return v, true
		}
	}
	return nil, false
}
