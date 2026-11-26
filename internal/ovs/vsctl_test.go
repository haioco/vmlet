package ovs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// fakeOVS implements the small slice of ovs-vsctl behaviour this package relies
// on, so tests run without Open vSwitch installed (and without root).
type fakeOVS struct {
	mu       sync.Mutex
	bridges  map[string][]string
	ports    map[string]string // port -> bridge
	extIDs   map[string]map[string]string
	commands []string
	failOn   map[string]int // command prefix -> remaining failures
}

func newFakeOVS() *fakeOVS {
	return &fakeOVS{
		bridges: map[string][]string{},
		ports:   map[string]string{},
		extIDs:  map[string]map[string]string{},
		failOn:  map[string]int{},
	}
}

// failNext makes the next n commands starting with prefix fail.
func (f *fakeOVS) failNext(prefix string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn[prefix] = n
}

// countPrefix returns how many recorded commands start with prefix.
func (f *fakeOVS) countPrefix(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, cmd := range f.commands {
		if strings.HasPrefix(cmd, prefix) {
			count++
		}
	}
	return count
}

// commandCount returns the total number of executed commands.
func (f *fakeOVS) commandCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commands)
}

func (f *fakeOVS) run(_ context.Context, _ string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.commands = append(f.commands, strings.Join(args, " "))

	for prefix, remaining := range f.failOn {
		if remaining > 0 && strings.Contains(strings.Join(args, " "), prefix) {
			f.failOn[prefix] = remaining - 1
			return "", &ExitError{
				Binary: "ovs-vsctl",
				Args:   args,
				Code:   1,
				Err:    errors.New("injected failure"),
			}
		}
	}

	// Global flags do not change the outcome for the fake.
	if args[0] == "--may-exist" || args[0] == "--if-exists" {
		return f.dispatch(args[1:]...)
	}
	return f.dispatch(args...)
}

// countContains returns how many recorded commands contain substring.
// ovs-vsctl invocations carry --may-exist/--if-exists flags, so matching on
// "contains" rather than a prefix is the honest way to count real actions.
func (f *fakeOVS) countContains(substring string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, cmd := range f.commands {
		if strings.Contains(cmd, substring) {
			count++
		}
	}
	return count
}

// dispatch applies a recorded command against the fake OVS state.
func (f *fakeOVS) dispatch(args ...string) (string, error) {
	switch args[0] {
	case "br-exists":
		if _, ok := f.bridges[args[1]]; ok {
			return "", nil
		}
		return "", &ExitError{Code: 2, Err: fmt.Errorf("no such bridge %q", args[1])}
	case "add-br":
		f.bridges[args[1]] = nil
		return "", nil
	case "del-br":
		delete(f.bridges, args[1])
		for port, bridge := range f.ports {
			if bridge == args[1] {
				delete(f.ports, port)
			}
		}
		return "", nil
	case "list-ports":
		return strings.Join(f.bridges[args[1]], "\n"), nil
	case "add-port":
		bridge, port := args[len(args)-2], args[len(args)-1]
		f.ports[port] = bridge
		f.bridges[bridge] = append(f.bridges[bridge], port)
		return "", nil
	case "del-port":
		port := args[len(args)-1]
		if bridge, ok := f.ports[port]; ok {
			f.bridges[bridge] = removeString(f.bridges[bridge], port)
			delete(f.ports, port)
		}
		return "", nil
	case "set":
		key, value := parseSetArg(args[len(args)-1])
		row := args[len(args)-2]
		if f.extIDs[row] == nil {
			f.extIDs[row] = map[string]string{}
		}
		f.extIDs[row][key] = value
		return "", nil
	case "clear":
		// Detaching a column: ovs-vsctl clear port <p> qos. Record it so tests
		// can assert the clearing happened.
		f.commands = append(f.commands, "applied-clear")
		return "", nil
	case "get":
		row, key := args[len(args)-2], args[len(args)-1]
		value, ok := f.extIDs[row][strings.TrimPrefix(key, "external_ids:")]
		if !ok {
			return "", &ExitError{Code: 1, Err: fmt.Errorf("no value for %s", key)}
		}
		return fmt.Sprintf("%q", value), nil
	}
	return "", fmt.Errorf("fakeOVS: unhandled command %q", args[0])
}

func fakeClient(f *fakeOVS) *Client {
	return NewClientWithRunner(DefaultBinary, f.run)
}

func parseSetArg(arg string) (string, string) {
	parts := strings.SplitN(arg, "=", 2)
	if len(parts) != 2 {
		return arg, ""
	}
	return strings.TrimPrefix(parts[0], "external_ids:"), parts[1]
}

func removeString(list []string, value string) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item != value {
			out = append(out, item)
		}
	}
	return out
}
