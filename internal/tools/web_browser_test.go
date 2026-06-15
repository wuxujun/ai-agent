package tools

import (
	"testing"
)

func TestRenderHTML(t *testing.T) {
	htmlInput := `<html>
<head>
    <title>Test Title</title>
    <style>body { color: red; }</style>
</head>
<body>
    <h1>Header 1</h1>
    <script>console.log("hello");</script>
    <noscript>Please enable JS</noscript>
    <p>This is a paragraph with a <a href="https://example.com">link</a> inside.</p>
    <iframe>Hidden contents</iframe>
</body>
</html>`

	expected := "Header 1\nThis is a paragraph with a [link](https://example.com) inside."
	output := renderHTML(htmlInput)

	if output != expected {
		t.Errorf("Expected:\n%q\nGot:\n%q", expected, output)
	}
}
