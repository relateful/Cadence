//go:build native

package voice

import "github.com/disgoorg/godave/golibdave"

func init() {
	daveFactory = golibdave.NewSession
}
