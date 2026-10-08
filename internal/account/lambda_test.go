package account

import (
	"fmt"
	"testing"
)

func TestScheduleID(t *testing.T) {
	desc := fmt.Sprintf("Runs bedrock-monthly-unpause at 06:00 UTC on the 1st (%s%s)", scheduleIDPrefix, "e2e")
	for in, want := range map[string]string{desc: "e2e", "made by hand": "", "": "", "bedrock-admin:id=x": "x"} {
		if got := ScheduleID(in); got != want {
			t.Errorf("ScheduleID(%q) = %q, want %q", in, got, want)
		}
	}
}
