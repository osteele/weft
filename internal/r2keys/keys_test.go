package r2keys

import "testing"

func TestCoordinatorRelayKeys(t *testing.T) {
	if got := CoordinatorRelayRequest("req-1"); got != "coordinator/v1/inbox/req-1.json" {
		t.Fatalf("CoordinatorRelayRequest = %q", got)
	}
	if got := CoordinatorRelayAck("req-1"); got != "coordinator/v1/acks/req-1.json" {
		t.Fatalf("CoordinatorRelayAck = %q", got)
	}
	if got := CoordinatorRelayInboxPrefix(); got != "coordinator/v1/inbox/" {
		t.Fatalf("CoordinatorRelayInboxPrefix = %q", got)
	}
	if got := CoordinatorRelayAckPrefix(); got != "coordinator/v1/acks/" {
		t.Fatalf("CoordinatorRelayAckPrefix = %q", got)
	}
}
