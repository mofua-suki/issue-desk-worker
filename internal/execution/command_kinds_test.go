package execution

import (
	"testing"
)

func TestCommandKindsHaveSourcedHandlers(t *testing.T) {
	// The intake enqueue implementation is outside this repository. This list is
	// the requester-provided contract, not a claim to discover future intake kinds.
	expected := map[string]bool{"stop": false, "instruction": false, "repost_proposal": true}
	if len(commandKinds) != len(expected) {
		t.Fatal("update the requester contract comparison")
	}
	for kind, repost := range expected {
		handler, ok := commandKinds[kind]
		if !ok || handler.Source == "" || handler.Repost != repost {
			t.Fatalf("missing handler/source for %s", kind)
		}
	}
}
func TestUnknownCommandKindsRemainUnacked(t *testing.T) {
	e, acks := commandExecutor(t, []command{{ID: "unknown", Kind: "future_kind"}}, false)
	if err := e.pollCommands(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *acks != 0 {
		t.Fatal("unknown kind acked")
	}
}
