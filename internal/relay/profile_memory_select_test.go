package relay

import (
	"fmt"
	"strings"
	"testing"
)

func mem(id, content string, pinned bool, updated string) profileMemory {
	return profileMemory{ID: id, Scope: "operator", Content: content, Pinned: pinned, UpdatedAt: updated}
}

// An older relevant note is recalled although newer irrelevant notes would have
// filled the whole prompt budget.
func TestOlderRelevantMemorySurvivesNewerIrrelevantOverflow(t *testing.T) {
	items := []profileMemory{}
	for i := 0; i < 200; i++ {
		items = append(items, mem(fmt.Sprintf("n%03d", i), "unrelated gardening note "+strings.Repeat("x", 100), false, fmt.Sprintf("2026-11-%02dT00:00:00Z", 1+i%28)))
	}
	items = append(items, mem("old", "invoice 4417 failed because the webhook secret rotated", false, "2020-01-01T00:00:00Z"))
	sel := selectMemories(items, "why did invoice 4417 fail?", memoryPromptBudget)
	out := renderMemorySelection(sel)
	if !strings.Contains(out, "webhook secret rotated") || len(sel.Included) != 1 {
		t.Fatalf("relevant memory not recalled: %+v", sel)
	}
	if sel.OmittedUnused != 200 || strings.Contains(out, "gardening") {
		t.Fatalf("irrelevant memory was injected to fill the budget: %+v", sel)
	}
	if len(out) > memoryPromptBudget {
		t.Fatalf("budget exceeded: %d", len(out))
	}
}

func TestPinnedOverflowAndBudgetOmissionAreReported(t *testing.T) {
	items := []profileMemory{}
	for i := 0; i < 20; i++ {
		items = append(items, mem(fmt.Sprintf("p%02d", i), "always keep policy "+strings.Repeat("y", 900), true, "2026-11-01T00:00:00Z"))
	}
	sel := selectMemories(items, "anything", memoryPromptBudget)
	out := renderMemorySelection(sel)
	if sel.PinnedOverflow == 0 || sel.Pinned == 0 || !strings.Contains(out, fmt.Sprintf("%d pinned omitted for budget", sel.PinnedOverflow)) {
		t.Fatalf("pinned overflow is not visible: %+v / %s", sel, out)
	}
	if len(out) > memoryPromptBudget {
		t.Fatalf("budget exceeded: %d", len(out))
	}
}

func TestMemorySelectionIsStableAndSecretsAreRedacted(t *testing.T) {
	items := []profileMemory{
		mem("b", "refund policy thirty days", false, "2026-11-01T00:00:00Z"),
		mem("a", "refund policy thirty days", false, "2026-11-01T00:00:00Z"),
		mem("c", "refund token sk-abcdefghijklmnopqrstuvwx123456 policy", false, "2026-11-02T00:00:00Z"),
	}
	first := selectMemories(items, "refund policy", memoryPromptBudget)
	second := selectMemories([]profileMemory{items[2], items[1], items[0]}, "refund policy", memoryPromptBudget)
	ids := func(s memorySelection) string {
		out := ""
		for _, m := range s.Included {
			out += m.ID
		}
		return out
	}
	if ids(first) != ids(second) || ids(first) != "cab" {
		t.Fatalf("unstable order: %s vs %s", ids(first), ids(second))
	}
	if strings.Contains(renderMemorySelection(first), "sk-abcdefghijklmnopqrstuvwx123456") {
		t.Fatal("secret-shaped content was injected unredacted")
	}
}
