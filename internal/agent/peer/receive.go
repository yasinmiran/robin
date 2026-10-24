package peer

import (
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/webrtc/v4"
)

// addReceiverFeedback adds what the agent owes the browser for media it
// sends: transport-wide feedback, without which the browser's congestion
// controller has nothing to go on, and NACKs for the camera's lost
// packets. Both act on remote streams only; the agent's own sending keeps
// its own NACK responder and no pacer.
func addReceiverFeedback(reg *interceptor.Registry) error {
	for _, newFactory := range []func() (interceptor.Factory, error){
		func() (interceptor.Factory, error) { return nack.NewGeneratorInterceptor() },
		func() (interceptor.Factory, error) { return twcc.NewSenderInterceptor() },
	} {
		f, err := newFactory()
		if err != nil {
			return err
		}
		reg.Add(f)
	}
	return nil
}

// drainRTCP reads what the browser says about a track it sends, so the
// interceptors see its sender reports, until the receiver stops.
func drainRTCP(r *webrtc.RTPReceiver) {
	for {
		if _, _, err := r.ReadRTCP(); err != nil {
			return
		}
	}
}
