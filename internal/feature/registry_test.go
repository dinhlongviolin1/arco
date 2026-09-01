package feature

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func cmd(name string) *Command {
	return &Command{Name: name, Help: name + " help", Run: func(context.Context, CmdInput) (string, error) { return "ok", nil }}
}

func TestRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Feature{Name: "scan", Command: cmd("scan")}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// slash-optional, case-insensitive lookup
	for _, q := range []string{"scan", "/scan", "SCAN", " /Scan "} {
		if _, ok := r.Command(q); !ok {
			t.Errorf("Command(%q) miss, want hit", q)
		}
	}
	if _, ok := r.Command("nope"); ok {
		t.Error("Command(nope) hit, want miss")
	}
}

func TestRegisterCanonicalizesName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Feature{Name: "x", Command: cmd("/Scan")}); err != nil {
		t.Fatal(err)
	}
	cmds := r.Commands()
	if len(cmds) != 1 || cmds[0].Name != "scan" {
		t.Fatalf("Commands()[0].Name = %q, want canonical \"scan\"", cmds[0].Name)
	}
	// still resolvable by any variant
	for _, q := range []string{"scan", "/scan", "SCAN"} {
		if _, ok := r.Command(q); !ok {
			t.Errorf("Command(%q) miss after canonicalization", q)
		}
	}
}

func TestEmptyRegistryIsInert(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Command("scan"); ok {
		t.Error("empty registry resolved a command")
	}
	if len(r.Commands()) != 0 || len(r.Tools()) != 0 || len(r.ForBrain()) != 0 {
		t.Error("empty registry is not empty")
	}
}

func TestDuplicateCommandRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Feature{Name: "a", Command: cmd("scan")}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(Feature{Name: "b", Command: cmd("/SCAN")}) // same name, different form
	if err == nil {
		t.Fatal("want duplicate-command error, got nil")
	}
}

func TestDuplicateToolRejected(t *testing.T) {
	r := NewRegistry()
	tool := &Tool{Name: "peek", Access: BrainSafe, Call: func(context.Context, json.RawMessage) (string, error) { return "", nil }}
	if err := r.Register(Feature{Name: "a", Tool: tool}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Feature{Name: "b", Tool: &Tool{Name: "peek", Call: tool.Call}}); err == nil {
		t.Fatal("want duplicate-tool error, got nil")
	}
}

func TestEmptyNamesRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Feature{Name: "x", Command: &Command{Name: "  "}}); err == nil {
		t.Error("empty command name accepted")
	}
	if err := r.Register(Feature{Name: "y", Tool: &Tool{Name: ""}}); err == nil {
		t.Error("empty tool name accepted")
	}
}

func TestForBrainExcludesOperatorTools(t *testing.T) {
	r := NewRegistry()
	mk := func(name string, a Access) Feature {
		return Feature{Name: name, Tool: &Tool{Name: name, Access: a,
			Call: func(context.Context, json.RawMessage) (string, error) { return "", nil }}}
	}
	r.MustRegister(mk("scan", BrainSafe), mk("dispatch", BrainAct), mk("grant", Operator))
	brain := r.ForBrain()
	if len(brain) != 2 {
		t.Fatalf("ForBrain len=%d, want 2 (BrainSafe+BrainAct, not Operator)", len(brain))
	}
	for _, tl := range brain {
		if tl.Access == Operator {
			t.Errorf("ForBrain leaked an Operator tool: %s", tl.Name)
		}
	}
	if len(r.Tools()) != 3 {
		t.Errorf("Tools len=%d, want 3 (all)", len(r.Tools()))
	}
}

func TestCommandsSorted(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(
		Feature{Name: "z", Command: cmd("zebra")},
		Feature{Name: "a", Command: cmd("apple")},
		Feature{Name: "m", Command: cmd("mango")},
	)
	got := r.Commands()
	want := []string{"apple", "mango", "zebra"}
	for i, c := range got {
		if c.Name != want[i] {
			t.Errorf("Commands()[%d]=%s, want %s", i, c.Name, want[i])
		}
	}
}

func TestMustRegisterPanicsOnDup(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustRegister did not panic on duplicate")
		}
	}()
	r := NewRegistry()
	r.MustRegister(Feature{Name: "a", Command: cmd("scan")}, Feature{Name: "b", Command: cmd("scan")})
}

// A Command.Run error round-trips as a normal error (no special wrapping).
func TestCommandRunError(t *testing.T) {
	sentinel := errors.New("boom")
	r := NewRegistry()
	r.MustRegister(Feature{Name: "x", Command: &Command{Name: "x",
		Run: func(context.Context, CmdInput) (string, error) { return "", sentinel }}})
	c, _ := r.Command("x")
	if _, err := c.Run(context.Background(), CmdInput{}); !errors.Is(err, sentinel) {
		t.Errorf("Run err=%v, want sentinel", err)
	}
}
