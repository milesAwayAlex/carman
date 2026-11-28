package meter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Listener is one of the meter's servers and where it listens. The first
// one given to Serve is the meter's own: if it fails, the meter ends.
type Listener struct {
	Server   *http.Server
	Listener net.Listener
}

// Serve runs the listeners and the sender until ctx ends (the pod is told
// to end). Then it waits up to drain for the answers under way, so that
// they are counted, and sends all the ledger holds, trying for up to
// final: a restart of the meter loses no tally.
func Serve(ctx context.Context, s *Sender, drain, final time.Duration, ls ...Listener) error {
	sending, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	done := make(chan struct{})
	go func() { s.Run(sending); close(done) }()

	failed := make(chan error, 1)
	for i, l := range ls {
		go func() {
			err := l.Server.Serve(l.Listener)
			if errors.Is(err, http.ErrServerClosed) {
				return
			}
			if i == 0 {
				failed <- err
				return
			}
			s.log().Error("a listener of the meter's stopped", "listen", l.Listener.Addr().String(), "error", err)
		}()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-failed:
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
	for _, l := range ls {
		_ = l.Server.Shutdown(dctx)
	}
	cancel()
	stop()
	<-done
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), final)
	defer cancel()
	s.Final(fctx)
	return err
}
