package api

import "testing"

func TestScalarClock(t *testing.T) {
	clock := Scalarclock(10)
	refclk := Scalarclock(20)
	if rclock := clock.Update(nil); rclock != clock {
		t.Errorf("expected %v, got %v", clock, rclock)
	} else if rclock := clock.Update(10); rclock != refclk {
		t.Errorf("expected %v, got %v", refclk, rclock)
	} else if rclock := clock.Clone(); rclock != clock {
		t.Errorf("expected %v, got %v", clock, rclock)
	} else if ok := clock.Less(Scalarclock(10)); ok == true {
		t.Errorf("expected %v, got %v", false, true)
	} else if ok := clock.Less(Scalarclock(9)); ok == true {
		t.Errorf("expected %v, got %v", true, ok)
	} else if ok := clock.LessEqual(Scalarclock(10)); ok == false {
		t.Errorf("expected %v, got %v", true, false)
	}
}
