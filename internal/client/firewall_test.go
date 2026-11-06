package client

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A keyed PUT that gives up may have been applied: the user is sent to another plan, not to look for a created resource.
func TestGiveUp_AKeyedPutSaysToPlanAgain(t *testing.T) {
	c := failWith(t, 502, "bad_gateway", "the upstream is down")
	c.retry.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := c.SetFirewall(context.Background(), "vm-1", nil, "key-1")
	apiErr := asAPIError(t, err)
	if !apiErr.MayHaveBeenApplied {
		t.Fatal("a keyed PUT that gave up after 502s may have been applied")
	}
	if text := apiErr.Error(); strings.Contains(text, "tofu import") || !strings.Contains(text, "run another plan to see the current state") {
		t.Errorf("Error() = %q, want the plan hint and no import hint", text)
	}
}
