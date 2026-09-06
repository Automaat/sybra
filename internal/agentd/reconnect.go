package agentd

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"time"
)

// Registration is a durable, idempotent operation. A leader restart or a
// sleeping laptop must not turn a transient transport failure into a daemon
// crash loop. Retain the registration identity and surviving runs while waiting.
func (d *Daemon) registerUntilReady(ctx context.Context) error {
	return d.retryRegistration(ctx, time.Second, 30*time.Second)
}

func (d *Daemon) retryRegistration(ctx context.Context, initial, maximum time.Duration) error {
	delay := initial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := d.register(ctx)
		if err == nil {
			d.logger.Info("agentd.connected")
			return nil
		}
		if !retryableLeaderError(err) {
			return err
		}
		// Equal jitter avoids synchronized retries while maintaining a bound.
		wait := delay/2 + time.Duration(rand.Int64N(max(int64(delay/2), 1)))
		d.logger.Warn("agentd.waiting_for_leader", "retry_after", wait, "err", err)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, maximum)
	}
}

func retryableLeaderError(err error) bool {
	// http.Client wraps these in *url.Error, which implements net.Error even
	// though retrying cannot repair trust or a mismatched server identity.
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return false
	}
	var response *leaderHTTPError
	if errors.As(err, &response) {
		return response.status == http.StatusRequestTimeout ||
			response.status == http.StatusTooManyRequests ||
			response.status >= http.StatusInternalServerError || isRejectedSession(err)
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
