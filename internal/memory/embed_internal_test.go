package memory

import (
	"errors"
	"testing"
)

func TestIsUnsupportedLocationError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "gemini explicit unsupported location",
			err:  errors.New("Error 400, Message: User location is not supported for the API use., Status: FAILED_PRECONDITION"),
			want: true,
		},
		{
			name: "failed precondition with location",
			err:  errors.New("Status: FAILED_PRECONDITION, Details: location blocked"),
			want: true,
		},
		{
			name: "ordinary remote failure",
			err:  errors.New("status 500: temporary server error"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUnsupportedLocationError(tt.err); got != tt.want {
				t.Fatalf("isUnsupportedLocationError() = %v, want %v", got, tt.want)
			}
		})
	}
}
