package inventoryqueue

import "testing"

func TestHostAddressedKeys(t *testing.T) {
	request, err := RequestKey("studio", "123-1")
	if err != nil {
		t.Fatal(err)
	}
	if request != "inventory/v1/hosts/studio/inbox/123-1.json" {
		t.Fatalf("RequestKey() = %q", request)
	}
	state, err := StateKey("studio")
	if err != nil {
		t.Fatal(err)
	}
	if state != "inventory/v1/hosts/studio/state.json" {
		t.Fatalf("StateKey() = %q", state)
	}
}

func TestKeysRejectPathSegments(t *testing.T) {
	if _, err := RequestKey("../studio", "request"); err == nil {
		t.Fatal("RequestKey accepted a host path traversal")
	}
	if _, err := RequestKey("studio", "../request"); err == nil {
		t.Fatal("RequestKey accepted a request path traversal")
	}
}
