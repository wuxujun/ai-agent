package store

import "testing"

func TestPGVectorLiteral(t *testing.T) {
	got := pgVectorLiteral([]float32{1, 0.25, -3.5})
	want := "[1,0.25,-3.5]"
	if got != want {
		t.Fatalf("pgVectorLiteral() = %q, want %q", got, want)
	}
}

func TestPGVectorLiteralEmpty(t *testing.T) {
	got := pgVectorLiteral(nil)
	want := "[]"
	if got != want {
		t.Fatalf("pgVectorLiteral(nil) = %q, want %q", got, want)
	}
}
