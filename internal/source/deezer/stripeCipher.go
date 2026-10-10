package deezer

import (
	"crypto/cipher"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/blowfish"
)

const (
	stripeChunk    = 2048
	stripeInterval = 3
	masterKeySize  = 16
)

var stripeIV = []byte{0, 1, 2, 3, 4, 5, 6, 7}

var ErrMasterKeySize = fmt.Errorf("DEEZER_MASTER_KEY must be exactly %d bytes", masterKeySize)

type StripeDecryptor struct {
	master []byte
}

func NewStripeDecryptor(masterKey string) (*StripeDecryptor, error) {
	if len(masterKey) != masterKeySize {
		return nil, ErrMasterKeySize
	}
	return &StripeDecryptor{master: []byte(masterKey)}, nil
}

func (s *StripeDecryptor) trackKey(trackID string) []byte {
	sum := md5.Sum([]byte(trackID))
	digest := hex.EncodeToString(sum[:])
	key := make([]byte, masterKeySize)
	for i := range key {
		key[i] = digest[i] ^ digest[i+masterKeySize] ^ s.master[i]
	}
	return key
}

func (s *StripeDecryptor) Decrypt(trackID string, encrypted io.Reader) (io.Reader, error) {
	return s.DecryptFrom(trackID, encrypted, 0)
}

func (s *StripeDecryptor) DecryptFrom(trackID string, encrypted io.Reader, firstChunk int) (io.Reader, error) {
	block, err := blowfish.NewCipher(s.trackKey(trackID))
	if err != nil {
		return nil, err
	}
	return &stripeReader{src: encrypted, block: block, chunk: make([]byte, stripeChunk), index: firstChunk}, nil
}

type stripeReader struct {
	src     io.Reader
	block   cipher.Block
	chunk   []byte
	pending []byte
	index   int
	err     error
}

func (r *stripeReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		r.fill()
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *stripeReader) fill() {
	n, err := io.ReadFull(r.src, r.chunk)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		err = io.EOF
	case err != nil && !errors.Is(err, io.EOF):
		err = fmt.Errorf("deezer stream: %w", err)
	}
	r.err = err
	if n == 0 {
		return
	}
	data := r.chunk[:n]
	if n == stripeChunk && r.index%stripeInterval == 0 {
		cipher.NewCBCDecrypter(r.block, stripeIV).CryptBlocks(data, data)
	}
	r.index++
	r.pending = data
}
