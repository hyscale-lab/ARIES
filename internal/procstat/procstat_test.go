package procstat

import (
	"os"
	"os/exec"
	"testing"
)

func TestStartTimeTellsALiveProcessFromAnExitedOne(t *testing.T) {
	start, alive, err := StartTime(os.Getpid())
	if err != nil || !alive || start == 0 {
		t.Fatalf("this process: start=%d alive=%v err=%v", start, alive, err)
	}
	again, _, _ := StartTime(os.Getpid())
	if again != start {
		t.Fatalf("start time changed: %d then %d", start, again)
	}
	command := exec.Command("true")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if _, alive, err := StartTime(command.Process.Pid); err != nil || alive {
		t.Fatalf("an exited process: alive=%v err=%v", alive, err)
	}
}
