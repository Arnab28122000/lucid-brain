package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
)

func TestRenderGroupIsOldestFirstAndStable(t *testing.T) {
	base := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	// Search returns newest first; the prompt must present the opposite, so the
	// model reads the sequence in the order it happened.
	g := store.ConsolidationGroup{
		TenantID: "acme",
		Kind:     memory.KindEvent,
		Topic:    "checkout-latency",
		Memories: []memory.Memory{
			{Statement: "Third alert.", ValidFrom: base.Add(2 * time.Hour)},
			{Statement: "Second alert.", ValidFrom: base.Add(time.Hour)},
			{Statement: "First alert.", ValidFrom: base, Attributes: map[string]string{
				"p99": "1.4s", "region": "eu-central-1", "alert": "checkout-latency",
			}},
		},
	}

	got := renderGroup(g)
	first := strings.Index(got, "First alert.")
	second := strings.Index(got, "Second alert.")
	third := strings.Index(got, "Third alert.")
	if !(first < second && second < third) {
		t.Errorf("memories are not oldest-first:\n%s", got)
	}

	// Attribute order must be deterministic or the prompt text changes between
	// runs and the gateway's prompt cache never hits.
	if got != renderGroup(g) {
		t.Error("renderGroup is not deterministic across calls")
	}
	if !strings.Contains(got, "alert=checkout-latency, p99=1.4s, region=eu-central-1") {
		t.Errorf("attributes are not sorted:\n%s", got)
	}
	if !strings.Contains(got, "2026-03-10") {
		t.Error("each memory should carry its date so the summary can be specific")
	}
}
