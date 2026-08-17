package wiki

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildWriteProposalCreatesStableDryRun(t *testing.T) {
	request := WriteProposalRequest{
		TaskID: "task-1", TargetURI: "wiki://local/concepts/pbl-guide",
		Content: "# PBL Guide\r\n\r\nNew content.\r\n",
	}
	first, err := BuildWriteProposal(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildWriteProposal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.DryRun || first.Operation != "create" || first.Space != "local" || first.Slug != "concepts/pbl-guide" {
		t.Fatalf("proposal = %+v", first)
	}
	if first.IdempotencyKey == "" || first.IdempotencyKey != second.IdempotencyKey || first.ContentHash != second.ContentHash {
		t.Fatalf("unstable proposals: first=%+v second=%+v", first, second)
	}
	if !strings.Contains(first.Diff, "+# PBL Guide") {
		t.Fatalf("diff = %q", first.Diff)
	}
}

func TestBuildWriteProposalDetectsNoopAndConflict(t *testing.T) {
	existing := "# Existing\n\nBody.\n"
	base, err := BuildWriteProposal(WriteProposalRequest{TaskID: "task-2", TargetURI: "wiki://tenant-a/entities/guide", ExistingContent: existing, Content: existing})
	if err != nil || base.Operation != "noop" || base.Diff != "" {
		t.Fatalf("base=%+v err=%v", base, err)
	}
	_, err = BuildWriteProposal(WriteProposalRequest{
		TaskID: "task-2", TargetURI: base.TargetURI, ExistingContent: "# Changed elsewhere\n", Content: "# Proposed\n",
		ExpectedExistingHash: base.ExistingHash,
	})
	if err == nil || !strings.Contains(err.Error(), "write conflict") {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestBuildWriteProposalValidatesTargetAndMarkdownSchema(t *testing.T) {
	tests := []WriteProposalRequest{
		{TaskID: "task", TargetURI: "https://example.test/concepts/page", Content: "# Page\n"},
		{TaskID: "task", TargetURI: "wiki://local/../../secret", Content: "# Page\n"},
		{TaskID: "task", TargetURI: "wiki://local/index/page", Content: "# Page\n"},
		{TaskID: "task", TargetURI: "wiki://local/concepts/page", Content: "body without title"},
		{TaskID: "task", TargetURI: "wiki://local/concepts/page", Content: "---\ntitle: [broken\n---\n"},
		{TaskID: "task", TargetURI: "wiki://local/concepts/page", Content: "---\ntitle: Page\n"},
		{TaskID: "", TargetURI: "wiki://local/concepts/page", Content: "# Page\n"},
	}
	for index, request := range tests {
		if _, err := BuildWriteProposal(request); err == nil {
			t.Errorf("case %d unexpectedly succeeded", index)
		}
	}
}

func TestBuildWriteProposalAcceptsFrontmatterTitleAndTruncatesDiff(t *testing.T) {
	content := "---\ntitle: Large Page\n---\n\n" + strings.Repeat("中文内容行\n", 7000)
	proposal, err := BuildWriteProposal(WriteProposalRequest{
		TaskID: "task-large", TargetURI: "wiki://local/sources/large-page", ExistingContent: "# Old\n", Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Operation != "update" || !proposal.DiffTruncated || !utf8.ValidString(proposal.Diff) || !strings.HasSuffix(proposal.Diff, "... (diff truncated)\n") {
		t.Fatalf("proposal = %+v", proposal)
	}
}
