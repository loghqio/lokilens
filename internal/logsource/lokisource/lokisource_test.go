package lokisource

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/lokilens/lokilens/internal/audit"
	"github.com/lokilens/lokilens/internal/safety"
)

func TestLokiSource_Name(t *testing.T) {
	s := &LokiSource{}
	if s.Name() != "Loki" {
		t.Errorf("expected Name()=Loki, got %q", s.Name())
	}
}

func TestLokiSource_Description(t *testing.T) {
	s := &LokiSource{}
	if s.Description() == "" {
		t.Error("expected non-empty description")
	}
}

func TestLokiSource_Instruction(t *testing.T) {
	s := &LokiSource{}
	instr := s.Instruction()
	if instr == "" {
		t.Error("expected non-empty instruction")
	}
	if !strings.Contains(instr, "LogQL") {
		t.Error("expected Loki instruction to contain LogQL references")
	}
}

func TestLokiSource_Tools(t *testing.T) {
	v := safety.NewValidator(24*60*60*1e9, 500)
	al := audit.New(slog.Default())

	// Pass nil loki client — we only test tool creation, not execution
	s := New(nil, v, al)
	tools, err := s.Tools()
	if err != nil {
		t.Fatalf("Tools() error: %v", err)
	}
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools, got %d", len(tools))
	}

	// Verify tool names
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name()] = true
	}
	for _, expected := range []string{"query_logs", "get_labels", "get_label_values", "query_stats"} {
		if !names[expected] {
			t.Errorf("missing tool %q", expected)
		}
	}
}
