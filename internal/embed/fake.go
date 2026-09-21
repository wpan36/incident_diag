package embed

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand"
	"sync"
)

// Fake is a deterministic Embedder.
//
// It ships in the package rather than in a _test.go file because the code that
// most needs testing without a provider — the ingestion handler — lives
// somewhere else, and an embedtest package would be one more package for one
// type. It follows mq.FakeProducer in that.
//
// The same text always produces the same vector, and different texts produce
// different ones, so a test can assert that a particular chunk was indexed with
// the vector that belongs to it rather than merely with a vector.
type Fake struct {
	mu    sync.Mutex
	calls int
	texts []string

	// Err, when set, is returned by every call and nothing is recorded. It is
	// how a test says "the provider is down".
	Err error

	// Dims overrides the vector length. It exists for the one test that has to
	// produce a wrong-length vector; zero means Dimensions.
	Dims int
}

// Embed implements Embedder.
func (f *Fake) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	if f.Err != nil {
		return nil, f.Err
	}
	f.texts = append(f.texts, texts...)

	dims := f.Dims
	if dims == 0 {
		dims = Dimensions
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = FakeVector(text, dims)
	}
	return out, nil
}

// Calls returns how many times Embed was called, which is how a test checks
// that batching happened.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Texts returns every text passed to Embed, in order.
func (f *Fake) Texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

// FakeVector is the deterministic vector Fake produces for a text.
//
// It is unit length, so a cosine search over fake vectors behaves like a cosine
// search over real ones: the nearest neighbour of a text is itself.
func FakeVector(text string, dims int) []float32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	r := rand.New(rand.NewSource(int64(h.Sum64())))

	v := make([]float32, dims)
	var norm float64
	for i := range v {
		x := r.NormFloat64()
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
