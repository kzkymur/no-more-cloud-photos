package admin

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/metadata"
)

// The sized RPO/RTO dataset uploads must be accepted by the real content
// detector as image/bmp and be pairwise distinct (SHA uniqueness).
func TestLiveSizedDatasetBMPsAreDistinctDetectedBMP(t *testing.T) {
	seen := map[[32]byte]bool{}
	for i := uint32(0); i < 64; i++ {
		data := liveBMP(i + 0x9e3779b9)
		got, err := metadata.Detect(bytes.NewReader(data), int64(len(data)))
		if err != nil || got.Format.MIMEType != "image/bmp" {
			t.Fatalf("seed %d: (%+v, %v), want image/bmp", i, got, err)
		}
		sum := sha256.Sum256(data)
		if seen[sum] {
			t.Fatalf("seed %d duplicates an earlier original", i)
		}
		seen[sum] = true
	}
	if liveMediaCount(2) != "3002" {
		t.Fatal("sized dataset offset")
	}
}
