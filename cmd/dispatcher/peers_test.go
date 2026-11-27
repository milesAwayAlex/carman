package main

import (
	"strings"
	"testing"

	"carman/internal/dispatch/message"
)

// TestR2PeersNamedAddress: a session's line starts with the address by
// name that reaches it, and keeps its id, shortened; one with no such name
// starts with the short id; what the caller may not write to says so.
func TestR2PeersNamedAddress(t *testing.T) {
	for _, c := range []struct {
		p    message.Peer
		me   string
		want []string
	}{
		{message.Peer{Address: "session/836e501a-1111-2222", Kind: "session", Agent: "host", Name: "carman", Named: "host/carman", State: "busy", Where: "interactive on the owner's machine"},
			"836e501a-1111-2222", []string{"host/carman ", "session/836e501a: busy, interactive on the owner's machine", "(you)"}},
		{message.Peer{Address: "session/95a4d403-1111", Kind: "session", Agent: "builder", State: "not running (kept)", Where: "last in pod-1"},
			"", []string{"session/95a4d403 ", "builder: not running (kept), last in pod-1"}},
		{message.Peer{Address: "agent/trial", Kind: "agent", Agent: "trial", State: "a message starts", NotWritable: "not writable by you"},
			"", []string{"agent/trial ", "trial: not writable by you"}},
	} {
		got := peerLine(c.p, c.me)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%q does not say %q", got, w)
			}
		}
	}
}
