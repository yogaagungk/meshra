package relay

import (
	"testing"
	"time"
)

func dstDefinition(zone, expr string) routineDefinition {
	return routineDefinition{Trigger: routineTrigger{Kind: "cron", Expression: expr, TimeZone: zone}}
}

func dstNext(t *testing.T, d routineDefinition, from time.Time, n int) []time.Time {
	t.Helper()
	out := []time.Time{}
	for range n {
		next, err := routineNextOccurrence(d, from)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, next)
		from = next
	}
	return out
}

func dstLocal(zone string, ts []time.Time) []string {
	loc, _ := time.LoadLocation(zone)
	out := []string{}
	for _, v := range ts {
		out = append(out, v.In(loc).Format("2006-01-02 15:04 MST"))
	}
	return out
}

func dstEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func dstTime(t *testing.T, value string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A daily local time keeps its wall-clock hour across both clock changes.
func TestRoutineCronKeepsLocalWallClockAcrossDST(t *testing.T) {
	d := dstDefinition("America/New_York", "0 9 * * *")
	got := dstLocal("America/New_York", dstNext(t, d, dstTime(t, "2026-03-06T12:00:00Z"), 4))
	dstEqual(t, got, []string{"2026-03-06 09:00 EST", "2026-03-07 09:00 EST", "2026-03-08 09:00 EDT", "2026-03-09 09:00 EDT"})
	got = dstLocal("America/New_York", dstNext(t, d, dstTime(t, "2026-10-30T12:00:00Z"), 4))
	dstEqual(t, got, []string{"2026-10-30 09:00 EDT", "2026-10-31 09:00 EDT", "2026-11-01 09:00 EST", "2026-11-02 09:00 EST"})
}

// Spring forward: a local time that does not exist has no occurrence that day.
// The schedule resumes the next day; it never fires at a shifted time.
func TestRoutineCronSkipsANonexistentLocalTime(t *testing.T) {
	d := dstDefinition("America/New_York", "30 2 * * *")
	got := dstLocal("America/New_York", dstNext(t, d, dstTime(t, "2026-03-06T12:00:00Z"), 3))
	dstEqual(t, got, []string{"2026-03-07 02:30 EST", "2026-03-09 02:30 EDT", "2026-03-10 02:30 EDT"})
}

// Fall back: a repeated local time fires once, on its first pass, in both US and
// UK rules, for a daily job and for a job that runs every half hour.
func TestRoutineCronFiresOnceWhenLocalTimeRepeats(t *testing.T) {
	d := dstDefinition("America/New_York", "30 1 * * *")
	got := dstLocal("America/New_York", dstNext(t, d, dstTime(t, "2026-10-30T12:00:00Z"), 3))
	dstEqual(t, got, []string{"2026-10-31 01:30 EDT", "2026-11-01 01:30 EDT", "2026-11-02 01:30 EST"})
	d = dstDefinition("Europe/London", "30 1 * * *")
	got = dstLocal("Europe/London", dstNext(t, d, dstTime(t, "2026-10-23T12:00:00Z"), 4))
	dstEqual(t, got, []string{"2026-10-24 01:30 BST", "2026-10-25 01:30 BST", "2026-10-26 01:30 GMT", "2026-10-27 01:30 GMT"})
	d = dstDefinition("America/New_York", "0,30 0-2 * * *")
	got = dstLocal("America/New_York", dstNext(t, d, dstTime(t, "2026-11-01T03:00:00Z"), 8))
	dstEqual(t, got, []string{"2026-11-01 00:00 EDT", "2026-11-01 00:30 EDT", "2026-11-01 01:00 EDT", "2026-11-01 01:30 EDT", "2026-11-01 02:00 EST", "2026-11-01 02:30 EST", "2026-11-02 00:00 EST", "2026-11-02 00:30 EST"})
}

// Downtime across the repeated hour counts each local occurrence once, so a
// catch-up never runs or reports a duplicate for the second pass.
func TestRoutineDowntimeAcrossFallBackCountsOccurrencesOnce(t *testing.T) {
	d := dstDefinition("America/New_York", "30 1 * * *")
	due := dstTime(t, "2026-10-31T05:30:00Z") // 01:30 EDT on the 31st
	at := dstTime(t, "2026-11-02T12:00:00Z")
	first, last, count, next, err := routineOccurrences(d, due, at)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || !first.Equal(due) {
		t.Fatalf("count %d first %s", count, first)
	}
	dstEqual(t, dstLocal("America/New_York", []time.Time{last, next}), []string{"2026-11-02 01:30 EST", "2026-11-03 01:30 EST"})
}
