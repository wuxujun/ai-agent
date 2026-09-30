package wiki

import "testing"

func TestDirectoryBM25DropsWeakNoAnswerMatchBelowThreshold(t *testing.T) {
	pages := []directoryIndexPage{
		{slug: "alpha", title: "课程安排", titleText: "课程安排", slugText: "alpha", bodyText: "这是普通的页面"},
		{slug: "beta", title: "教师信息", titleText: "教师信息", slugText: "beta", bodyText: "这是普通的资料"},
	}
	index := buildDirectoryBM25Index(pages)
	docs, _, err := (&DirectoryClient{}).searchBM25("的", nil, "", "", 5, "local", pages, index, "test", false)
	if err != nil {
		t.Fatalf("searchBM25: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("weak no-answer query returned %d docs, want 0: %+v", len(docs), docs)
	}
}

func TestDirectoryBM25KeepsStrongRelevantMatchAboveThreshold(t *testing.T) {
	pages := []directoryIndexPage{
		{slug: "alpha", title: "课程安排", titleText: "课程安排", slugText: "alpha", bodyText: "这是普通的页面"},
		{slug: "beta", title: "教师信息", titleText: "教师信息", slugText: "beta", bodyText: "这是普通的资料"},
	}
	index := buildDirectoryBM25Index(pages)
	docs, _, err := (&DirectoryClient{}).searchBM25("教师信息", nil, "教师信息", "教师信息", 5, "local", pages, index, "test", false)
	if err != nil {
		t.Fatalf("searchBM25: %v", err)
	}
	if len(docs) == 0 || docs[0].Slug != "beta" {
		t.Fatalf("strong query result = %+v, want beta first", docs)
	}
}
