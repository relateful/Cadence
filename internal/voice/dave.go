package voice

import (
	"errors"
	"log/slog"

	"github.com/disgoorg/godave"
)

var ErrNoDave = errors.New("cadence was built without the native tag, so DAVE is unavailable and Discord will reject voice")

var daveFactory godave.SessionCreateFunc

func DaveAvailable() bool {
	return daveFactory != nil
}

func UseDaveFactory(f godave.SessionCreateFunc) {
	daveFactory = f
}

func newDave(log *slog.Logger, userID string, cb godave.Callbacks) (godave.Session, error) {
	if daveFactory == nil {
		return nil, ErrNoDave
	}
	return daveFactory(log, godave.UserID(userID), cb), nil
}
