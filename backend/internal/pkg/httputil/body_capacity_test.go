package httputil

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// trickleReader returns at most chunk bytes per Read, like a socket.
type trickleReader struct {
	r     io.Reader
	chunk int
}

func (t *trickleReader) Read(p []byte) (int, error) {
	if len(p) > t.chunk {
		p = p[:t.chunk]
	}
	return t.r.Read(p)
}

func bodyPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func TestReadBodyWithLengthHint_ExactCapacityForLargeHonestLength(t *testing.T) {
	for _, n := range []int{requestBodyReadMaxInitCap + 1, 6*requestBodyReadMaxInitCap + 12345, 8 * requestBodyReadMaxInitCap} {
		want := bodyPattern(n)
		got, err := readBodyWithLengthHint(&trickleReader{r: bytes.NewReader(want), chunk: 32 << 10}, int64(n))
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("n=%d: body mismatch", n)
		}
		if cap(got) != n {
			t.Fatalf("n=%d: retained cap %d, want exactly the declared length", n, cap(got))
		}
	}
}

func TestReadBodyWithLengthHint_ForgedLengthDoesNotPreallocate(t *testing.T) {
	want := bodyPattern(3 << 20)
	got, err := readBodyWithLengthHint(bytes.NewReader(want), 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("body mismatch")
	}
	if cap(got) > 2*len(want) {
		t.Fatalf("cap %d exceeds 2x received bytes %d", cap(got), len(want))
	}
}

func TestReadBodyWithLengthHint_UnderstatedAndMissingLength(t *testing.T) {
	want := bodyPattern(5<<20 + 7)
	for _, hint := range []int64{-1, 0, 2 << 20} {
		got, err := readBodyWithLengthHint(&trickleReader{r: bytes.NewReader(want), chunk: 1 << 20}, hint)
		if err != nil {
			t.Fatalf("hint=%d: %v", hint, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("hint=%d: body mismatch", hint)
		}
	}
	empty, err := readBodyWithLengthHint(bytes.NewReader(nil), 0)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty body: len=%d err=%v", len(empty), err)
	}
}

func TestReadRequestBodyWithPrealloc_PropagatesMaxBytesError(t *testing.T) {
	payload := bodyPattern(3 << 20)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, 2<<20)
	_, err := ReadRequestBodyWithPrealloc(req)
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		t.Fatalf("want *http.MaxBytesError, got %v", err)
	}
	// The limit is exactly the body length: still accepted.
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, int64(len(payload)))
	got, err := ReadRequestBodyWithPrealloc(req)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("limit == length: err=%v equal=%v", err, bytes.Equal(got, payload))
	}
}
