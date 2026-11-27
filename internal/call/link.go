// Package call is the link between the cloud call bridge and the Pi (C2):
// one WebSocket that carries 20 ms audio frames as binary messages and control
// messages as JSON text messages. The Pi (cmd/ghost-wispr) and the bridge
// (cmd/ghost-wispr-bridge) both use it: the frame codec (frame.go), the
// control messages (control.go), the link versions (versions.go), and the
// send queue each side's writer uses (queue.go).
package call

// Message is one thing a link delivers: a control message or an audio frame.
type Message struct {
	Control *Control
	Frame   *Frame
}

// PiLink is the bridge's view of its link to the Pi. The callfault build's
// fake Pi and the bridge's client implement it. At each link-up a PiLink puts
// the Pi's hello on Recv without waiting for the bridge's hello. The bridge
// answers it with its own hello through Send, before any other message. A
// link that writes through a SendQueue holds back whatever is sent before
// that hello until the hello has gone. Generation tells the link-ups apart:
// a link can drop and come back before the bridge takes the new hello, and
// what the dropped link took may be lost.
type PiLink interface {
	Hello() Hello            // the Pi's last hello; the zero Hello (no LinkVersions) before the first
	Send(c Control) error    // an error while the link is down
	SendAudio(f Frame) error // an error while the link is down
	Recv() <-chan Message    // the Pi's messages; after each link-up the first is the Pi's hello
	Up() bool
	Generation() uint64 // the link-ups so far: 0 before the first; one more at each link-up, before its hello is on Recv
}
