package admission

import (
	"testing"

	"github.com/opendatahub-io/ai-metering-service/internal/api"
)

func TestMostRestrictive(t *testing.T) {
	cases := []struct {
		a, b, want State
	}{
		{StateAllow, StateAllow, StateAllow},
		{StateAllow, StateOver, StateOver},
		{StateAllow, StateDeny, StateDeny},
		{StateOver, StateAllow, StateOver},
		{StateOver, StateOver, StateOver},
		{StateOver, StateDeny, StateDeny},
		{StateDeny, StateAllow, StateDeny},
		{StateDeny, StateOver, StateDeny},
		{StateDeny, StateDeny, StateDeny},
	}
	for _, c := range cases {
		if got := MostRestrictive(c.a, c.b); got != c.want {
			t.Errorf("MostRestrictive(%s, %s): expected %s, got %s", c.a, c.b, c.want, got)
		}
		// The fold must be order-independent.
		if got := MostRestrictive(c.b, c.a); got != c.want {
			t.Errorf("MostRestrictive(%s, %s): expected %s, got %s", c.b, c.a, c.want, got)
		}
	}
}

func TestOverageState(t *testing.T) {
	cases := []struct {
		overage *api.OverageConfig
		want    State
	}{
		{nil, StateDeny},
		{&api.OverageConfig{Type: "soft"}, StateOver},
		{&api.OverageConfig{Type: "group"}, StateDeny}, // fallback levels: non-goal, defense-in-depth
		{&api.OverageConfig{Type: "bogus"}, StateDeny}, // invalid rows: fail closed
	}
	for _, c := range cases {
		if got := OverageState(c.overage); got != c.want {
			t.Errorf("OverageState(%+v): expected %s, got %s", c.overage, c.want, got)
		}
	}
}

func TestStateForBalance(t *testing.T) {
	cases := []struct {
		balance int64
		overage *api.OverageConfig
		want    State
	}{
		{100, nil, StateAllow},
		{0, nil, StateAllow}, // a balance exactly on zero is still allowed
		{-1, nil, StateDeny},
		{-1000, &api.OverageConfig{Type: "soft"}, StateOver},
	}
	for _, c := range cases {
		if got := StateForBalance(c.balance, c.overage); got != c.want {
			t.Errorf("StateForBalance(%d, %+v): expected %s, got %s", c.balance, c.overage, c.want, got)
		}
	}
}
