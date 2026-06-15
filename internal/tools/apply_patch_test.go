package tools

import (
	"os"
	"testing"
)

func TestParseSearchReplacePatch(t *testing.T) {
	patchContent := `Some random text before patch.
<<<<<<< SEARCH
old line 1
old line 2
=======
new line 1
new line 2
>>>>>>> REPLACE
Some text after patch.`

	blocks, err := parseSearchReplacePatch(patchContent)
	if err != nil {
		t.Fatalf("expected no parsing error, got %v", err)
	}

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}

	if blocks[0].search != "old line 1\nold line 2" {
		t.Errorf("search block mismatch: got %q", blocks[0].search)
	}

	if blocks[0].replace != "new line 1\nnew line 2" {
		t.Errorf("replace block mismatch: got %q", blocks[0].replace)
	}
}

func TestApplySearchReplaceBlocks(t *testing.T) {
	tempFile, err := os.CreateTemp("", "test_patch_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tempFile.Name())

	originalContent := `line A
line B
line C
line D`

	if _, err := tempFile.WriteString(originalContent); err != nil {
		tempFile.Close()
		t.Fatal(err)
	}
	tempFile.Close()

	blocks := []patchBlock{
		{
			search:  "line B\nline C",
			replace: "line X\nline Y",
		},
	}

	if err := applySearchReplaceBlocks(tempFile.Name(), blocks); err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	res, err := os.ReadFile(tempFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	expectedContent := `line A
line X
line Y
line D`

	if string(res) != expectedContent {
		t.Errorf("Expected content:\n%q\nGot:\n%q", expectedContent, string(res))
	}
}

func TestApplySearchReplaceBlocks_NonUnique(t *testing.T) {
	tempFile, err := os.CreateTemp("", "test_patch_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tempFile.Name())

	originalContent := `line A
line B
line A
line D`

	if _, err := tempFile.WriteString(originalContent); err != nil {
		tempFile.Close()
		t.Fatal(err)
	}
	tempFile.Close()

	blocks := []patchBlock{
		{
			search:  "line A",
			replace: "line X",
		},
	}

	err = applySearchReplaceBlocks(tempFile.Name(), blocks)
	if err == nil {
		t.Fatal("expected error due to multiple occurrences, got none")
	}
}
