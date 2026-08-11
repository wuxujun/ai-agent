package tools

import (
	"reflect"
	"testing"
)

func TestSplitCommandArgs(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{name: "plain", input: "test ./...", want: []string{"test", "./..."}},
		{name: "python code", input: `-c "import json; print('ok value')"`, want: []string{"-c", "import json; print('ok value')"}},
		{name: "single quoted", input: `-c 'print("ok")'`, want: []string{"-c", `print("ok")`}},
		{name: "escaped space", input: `hello\ world`, want: []string{"hello world"}},
		{name: "empty quoted", input: `""`, want: []string{""}},
		{name: "unterminated", input: `-c "print(1)`, wantErr: true},
		{name: "trailing escape", input: `value\`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitCommandArgs(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("splitCommandArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("splitCommandArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
