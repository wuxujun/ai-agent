package multiagent

import (
	"fmt"
	"strings"
	"unicode"
)

const invalidAnswerFallback = "Answer generation failed because the model returned placeholder content. Please retry the task."

var placeholderAnswers = map[string]struct{}{
	"...":  {},
	"…":    {},
	"....": {},
	"n/a":  {},
	"na":   {},
	"none": {},
	"null": {},
	"ok":   {},
	"tbd":  {},
	"todo": {},
	"无":    {},
	"暂无":   {},
	"不知道":  {},
	"无法回答": {},
	"待补充":  {},
}

// validateGeneratedAnswer rejects responses that satisfy the JSON schema but
// do not contain a publishable answer. Keeping this check independent of the
// provider makes it cover Writer, reviewed Verifier, and resumed checkpoints.
func validateGeneratedAnswer(answer string) error {
	trimmed := strings.TrimSpace(answer)
	if trimmed == "" {
		return fmt.Errorf("final answer is empty")
	}
	if _, found := placeholderAnswers[strings.ToLower(trimmed)]; found {
		return fmt.Errorf("final answer is a known placeholder")
	}

	meaningful := 0
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			meaningful++
		}
	}
	if meaningful == 0 {
		return fmt.Errorf("final answer contains only punctuation or symbols")
	}
	if meaningful < 2 {
		return fmt.Errorf("final answer is too short")
	}
	return nil
}

func validateVerificationDraft(draft *VerificationDraft) error {
	if draft == nil {
		return fmt.Errorf("final verifier returned no candidate answer")
	}
	if err := validateGeneratedAnswer(draft.FinalAnswer); err != nil {
		return err
	}
	if strings.TrimSpace(draft.EvidenceSummary) == "" {
		return fmt.Errorf("final verifier returned an empty evidence summary")
	}
	return nil
}

func validateWriterOutput(output *WriterOutput) error {
	if output == nil {
		return fmt.Errorf("writer returned no result")
	}
	return validateGeneratedAnswer(output.FinalAnswer)
}

func validateFinalVerificationOutput(output *FinalVerificationOutput) error {
	if output == nil {
		return fmt.Errorf("final verifier returned no result")
	}
	return validateGeneratedAnswer(output.FinalAnswer)
}
