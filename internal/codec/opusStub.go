//go:build !native

package codec

func NewEncoder(int) (Encoder, error) {
	return nil, ErrNoNativeOpus
}
