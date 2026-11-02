package main

import (
	"reflect"
	"testing"
)

func TestSelectorForStrategy(t *testing.T) {
	roundRobin := newRoundRobinSelector()
	cases := []struct {
		strategy string
		want     Selector
		wantErr  bool
	}{
		{strategy: "", want: roundRobin},
		{strategy: StrategyRoundRobin, want: roundRobin},
		{strategy: StrategyFailover, want: failoverSelector{}},
		{strategy: "weighted", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.strategy, func(t *testing.T) {
			got, err := selectorForStrategy(roundRobin, tc.strategy)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("selectorForStrategy: %v", err)
			}
			if reflect.TypeOf(got) != reflect.TypeOf(tc.want) {
				t.Fatalf("selector type = %T, want %T", got, tc.want)
			}
		})
	}
}

func TestRoundRobinOrderVisitsEveryMemberOnce(t *testing.T) {
	selector := newRoundRobinSelector()
	combo := mustCombo(t, "c", StrategyRoundRobin, "a", "b", "c")

	first := selector.Order(combo, "")
	second := selector.Order(combo, "")
	if !reflect.DeepEqual(first, []int{0, 1, 2}) {
		t.Fatalf("first order = %v", first)
	}
	if !reflect.DeepEqual(second, []int{1, 2, 0}) {
		t.Fatalf("second order = %v", second)
	}
}

func TestRoundRobinCountersArePerCombo(t *testing.T) {
	selector := newRoundRobinSelector()
	one := mustCombo(t, "one", StrategyRoundRobin, "a", "b")
	two := mustCombo(t, "two", StrategyRoundRobin, "c", "d")

	_ = selector.Order(one, "")
	_ = selector.Order(one, "")
	if got := selector.Order(two, ""); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("other combo order = %v, want its own counter at 0", got)
	}
}

func TestFailoverOrderAlwaysStartsAtZero(t *testing.T) {
	combo := mustCombo(t, "c", StrategyFailover, "a", "b", "c")
	selector := failoverSelector{}
	for range 2 {
		if got := selector.Order(combo, ""); !reflect.DeepEqual(got, []int{0, 1, 2}) {
			t.Fatalf("order = %v", got)
		}
	}
}

func TestWithPreferred(t *testing.T) {
	order := []int{1, 2, 0}
	if got := withPreferred(order, -1); !reflect.DeepEqual(got, order) {
		t.Fatalf("preferred -1 = %v", got)
	}
	if got := withPreferred(order, 2); !reflect.DeepEqual(got, []int{2, 1, 0}) {
		t.Fatalf("preferred 2 = %v", got)
	}
	if got := withPreferred(order, 9); !reflect.DeepEqual(got, order) {
		t.Fatalf("stale preferred = %v", got)
	}
	if got := withPreferred(order, 0); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("preferred 0 = %v", got)
	}
}
