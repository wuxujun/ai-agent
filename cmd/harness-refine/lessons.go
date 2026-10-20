package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/wuxujun/ai-agent/internal/improvement"
)

// runLessons implements the improvement-harness operator commands:
//   - lessons list   --lessons-dir DIR [--team T] [--limit N]
//   - lessons retract --lessons-dir DIR --id ID --reason "..."
//   - assessments list --lessons-dir DIR [--team T] [--limit N]
func runLessons(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "lessons: want list, retract, or assessments")
		return 2
	}
	switch args[0] {
	case "list":
		return runLessonsList(args[1:], stdout, stderr)
	case "retract":
		return runLessonsRetract(args[1:], stdout, stderr)
	case "assessments":
		if len(args) > 1 && args[1] == "list" {
			return runAssessmentsList(args[2:], stdout, stderr)
		}
		fmt.Fprintln(stderr, "lessons: want 'assessments list'")
		return 2
	default:
		fmt.Fprintf(stderr, "lessons: unknown action %q\n", args[0])
		return 2
	}
}

func runAssessmentsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("assessments list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lessonsDir := fs.String("lessons-dir", "", "improvement harness directory (required)")
	team := fs.String("team", "", "filter by team (default: all)")
	limit := fs.Int("limit", 20, "max assessments to show")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *lessonsDir == "" {
		fmt.Fprintln(stderr, "assessments list: --lessons-dir is required")
		return 2
	}
	assessments, err := improvement.NewStore(*lessonsDir).ListAssessments(context.Background(), *team, *limit)
	if err != nil {
		fmt.Fprintf(stderr, "assessments list: %v\n", err)
		return 1
	}
	if len(assessments) == 0 {
		fmt.Fprintln(stdout, "no assessments recorded")
		return 0
	}
	for _, a := range assessments {
		fmt.Fprintf(stdout, "%s  team=%s  task=%s  status=%s  steps=%d errors=%d  candidates=%d  %s\n",
			a.ID[:12], a.Team, a.TaskID, a.TaskStatus, a.StepCount, a.ErrorSteps,
			len(a.Candidates), a.ObservedAt.Format("2006-01-02"))
	}
	return 0
}

func runLessonsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lessons list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lessonsDir := fs.String("lessons-dir", "", "improvement harness directory (required)")
	team := fs.String("team", "", "filter by team (default: all)")
	limit := fs.Int("limit", 20, "max lessons to show")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *lessonsDir == "" {
		fmt.Fprintln(stderr, "lessons list: --lessons-dir is required")
		return 2
	}
	lessons, err := improvement.NewStore(*lessonsDir).List(context.Background(), *team, *limit)
	if err != nil {
		fmt.Fprintf(stderr, "lessons list: %v\n", err)
		return 1
	}
	if len(lessons) == 0 {
		fmt.Fprintln(stdout, "no lessons recorded")
		return 0
	}
	for _, l := range lessons {
		fmt.Fprintf(stdout, "%s  team=%s  confidence=%.2f  %s  edits=%d  %s\n",
			l.ID[:12], l.Team, l.Confidence, l.ObservedAt.Format("2006-01-02"),
			len(l.Edits), l.ContrastDelta)
		for _, e := range l.Edits {
			fmt.Fprintf(stdout, "    %s %s -> %s\n", e.Op, e.Target, truncate(e.After, 60))
		}
	}
	return 0
}

func runLessonsRetract(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lessons retract", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lessonsDir := fs.String("lessons-dir", "", "improvement harness directory (required)")
	id := fs.String("id", "", "lesson ID (required)")
	reason := fs.String("reason", "", "retraction reason (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *lessonsDir == "" || *id == "" || *reason == "" {
		fmt.Fprintln(stderr, "lessons retract: --lessons-dir, --id and --reason are required")
		return 2
	}
	if err := improvement.NewStore(*lessonsDir).Retract(context.Background(), *id, *reason); err != nil {
		fmt.Fprintf(stderr, "lessons retract: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "retracted lesson %s (logical retraction; record kept on disk)\n", *id)
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
