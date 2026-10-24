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
// its own NACK responder and pacer.
func addReceiverFeedback(reg *interceptor.Registry) error {
	nacks, err := nack.NewGeneratorInterceptor()
	if err != nil {
		return err
	}
	reg.Add(nacks)
	feedback, err := twcc.NewSenderInterceptor()
	if err != nil {
		return err
	}
	reg.Add(feedback)
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
