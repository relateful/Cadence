package deezer

import (
	"bytes"
	"crypto/cipher"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"golang.org/x/crypto/blowfish"
)

const testMaster = "0123456789abcdef"

func stripeEncrypt(t *testing.T, d *StripeDecryptor, trackID string, plain []byte) []byte {
	block, err := blowfish.NewCipher(d.trackKey(trackID))
	if err != nil {
		t.Fatal(err)
	}
	out := bytes.Clone(plain)
	for i, off := 0, 0; off < len(out); i, off = i+1, off+stripeChunk {
		end := min(off+stripeChunk, len(out))
		if end-off == stripeChunk && i%stripeInterval == 0 {
			cipher.NewCBCEncrypter(block, stripeIV).CryptBlocks(out[off:end], out[off:end])
		}
	}
	return out
}

func TestStripeDecryptRoundTrips(t *testing.T) {
	d, err := NewStripeDecryptor(testMaster)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 100, stripeChunk, stripeChunk*3 + 7, stripeChunk * 7} {
		plain := make([]byte, size)
		for i := range plain {
			plain[i] = byte(i*31 + i/7)
		}
		enc := stripeEncrypt(t, d, "3135553", plain)
		if size >= stripeChunk && bytes.Equal(enc, plain) {
			t.Fatalf("size %d: nothing was encrypted", size)
		}
		r, err := d.Decrypt("3135553", iotest.HalfReader(bytes.NewReader(enc)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(iotest.OneByteReader(r))
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size %d: decrypted output differs", size)
		}
	}
}

func TestStripeKeyDependsOnTrack(t *testing.T) {
	d, _ := NewStripeDecryptor(testMaster)
	if bytes.Equal(d.trackKey("1"), d.trackKey("2")) {
		t.Fatal("tracks share a key")
	}
}

func TestStripeRejectsBadMasterKey(t *testing.T) {
	if _, err := NewStripeDecryptor("short"); !errors.Is(err, ErrMasterKeySize) {
		t.Fatalf("got %v", err)
	}
}

func TestStripeSurfacesReadErrors(t *testing.T) {
	d, _ := NewStripeDecryptor(testMaster)
	boom := errors.New("reset")
	r, _ := d.Decrypt("1", iotest.ErrReader(boom))
	if _, err := io.ReadAll(r); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}
