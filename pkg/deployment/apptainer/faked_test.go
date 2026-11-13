package apptainer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func answerFaked(fake *fakeApptainer, reply string, failure error) {
	fake.respond = func(_ context.Context, args []string, _ []byte, stdout, _ io.Writer) (bool, error) {
		if !slices.Contains(args, fakedShell) {
			return false, nil
		}
		_, _ = io.WriteString(stdout, reply)
		return true, failure
	}
}

func TestCreateGivesEveryCommandTheInstancesFakerootKey(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	answerFaked(fake, "12345:77\n", nil)
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	environment, err := os.ReadFile(filepath.Join(manager.sharePath(id), environmentFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(environment), "FAKEROOTKEY='12345'\n") || !strings.HasPrefix(string(environment), "A=") {
		t.Fatalf("environment = %q", environment)
	}
	path, err := manager.commandEnvironment(id, "token", map[string]string{"EXTRA": "x"})
	if err != nil {
		t.Fatal(err)
	}
	command, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(command), "FAKEROOTKEY='12345'\n") {
		t.Fatalf("command environment = %q", command)
	}
}

func TestCreateWithoutFakerootAddsNoKey(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	answerFaked(fake, "", nil)
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	environment, err := os.ReadFile(filepath.Join(manager.sharePath(id), environmentFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(environment), "FAKEROOTKEY") {
		t.Fatalf("environment = %q", environment)
	}
}

// A daemon that did not start leaves commands to hang later, so Create fails, and
// returns the identity Stop needs to remove the instance.
func TestCreateFailsWhenTheFakerootDaemonDoesNotStart(t *testing.T) {
	for name, answer := range map[string]struct {
		reply   string
		failure error
	}{
		"exec failed":  {"", errors.New("exit status 1")},
		"no separator": {"12345\n", nil},
		"not a key":    {"key:77\n", nil},
		"not a PID":    {"12345:pid\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			manager, fake, _ := newTestManager(t, Options{})
			answerFaked(fake, answer.reply, answer.failure)
			request := testRequest()
			id, err := manager.Create(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), "fakeroot daemon") {
				t.Fatalf("Create error = %v", err)
			}
			if id != request.Name {
				t.Fatalf("Create identity = %q", id)
			}
		})
	}
}
