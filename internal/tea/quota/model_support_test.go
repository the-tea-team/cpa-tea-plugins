package quota

import "testing"

// A pool whose members exist but carry no model inventory cannot answer a named model: that is the
// shape an early collection pass produces before the host publishes which models each credential
// serves, and callers must not read it as "the fleet shrank".
func TestPoolHasModelSupportDistinguishesUnreadableInventoryFromNoSupport(t *testing.T) {
	s := baseSnapshot()
	if !PoolHasModelSupport(s, s.Pools[0]) {
		t.Fatal("known model inventory must report support")
	}
	s.Credentials[0].Models = nil
	if PoolHasModelSupport(s, s.Pools[0]) {
		t.Fatal("a member with no model inventory must not report support")
	}
	s.Credentials = nil
	if PoolHasModelSupport(s, s.Pools[0]) {
		t.Fatal("an empty pool must not report support")
	}
}
