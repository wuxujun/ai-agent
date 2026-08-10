package llm

import (
	"strconv"
	"strings"
)

// JSONStringFieldStream incrementally extracts one JSON string field while
// suppressing every other field. It is intended for safely exposing selected
// user-facing text from structured LLM output.
type JSONStringFieldStream struct {
	field    string
	emit     func(string)
	buffer   strings.Builder
	rawValue strings.Builder
	emitted  int
	found    bool
	closed   bool
}

func NewJSONStringFieldStream(field string, emit func(string)) *JSONStringFieldStream {
	return &JSONStringFieldStream{field: strings.TrimSpace(field), emit: emit}
}

func (s *JSONStringFieldStream) Write(chunk string) {
	if s == nil || s.emit == nil || s.closed || chunk == "" || s.field == "" {
		return
	}
	if !s.found {
		s.buffer.WriteString(chunk)
		data := s.buffer.String()
		key := `"` + s.field + `"`
		searchFrom := 0
		for {
			index := strings.Index(data[searchFrom:], key)
			if index < 0 {
				return
			}
			index += searchFrom
			if index == 0 || data[index-1] != '\\' {
				afterKey := data[index+len(key):]
				colon := strings.IndexByte(afterKey, ':')
				if colon < 0 {
					return
				}
				afterColon := afterKey[colon+1:]
				quote := strings.IndexByte(afterColon, '"')
				if quote < 0 {
					return
				}
				s.found = true
				valueStart := index + len(key) + colon + 1 + quote + 1
				s.consume(data[valueStart:])
				return
			}
			searchFrom = index + len(key)
		}
	}
	s.consume(chunk)
}

func (s *JSONStringFieldStream) consume(data string) {
	for _, char := range data {
		raw := s.rawValue.String()
		if char == '"' && !endsWithOddBackslashes(raw) {
			s.closed = true
			s.emitDecoded()
			return
		}
		s.rawValue.WriteRune(char)
	}
	s.emitDecoded()
}

func (s *JSONStringFieldStream) emitDecoded() {
	decoded, err := strconv.Unquote(`"` + s.rawValue.String() + `"`)
	if err != nil || len(decoded) <= s.emitted {
		return
	}
	s.emit(decoded[s.emitted:])
	s.emitted = len(decoded)
}

func endsWithOddBackslashes(value string) bool {
	count := 0
	for index := len(value) - 1; index >= 0 && value[index] == '\\'; index-- {
		count++
	}
	return count%2 == 1
}
