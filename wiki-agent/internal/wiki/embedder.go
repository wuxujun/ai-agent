package wiki

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// HashEmbedder is a deterministic, dependency-free feature-hashing embedder.
// It captures lexical overlap only and is intended for local development and
// tests; production deployments should plug in a model-backed Embedder.
type HashEmbedder struct {
	Dims int
}

func (e HashEmbedder) Model() string   { return "hash-bow-v1" }
func (e HashEmbedder) Dimensions() int { return e.Dims }

// Embed implements Embedder. Output vectors are L2-normalised.
func (e HashEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v := make([]float32, e.Dims)
		for _, tok := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			h := fnv.New64a()
			_, _ = h.Write([]byte(tok))
			sum := h.Sum64()
			sign := float32(1)
			if sum&1 == 1 {
				sign = -1
			}
			v[(sum>>1)%uint64(e.Dims)] += sign
		}
		normalize(v)
		out[i] = v
	}
	return out, nil
}

func normalize(v []float32) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= inv
	}
}
