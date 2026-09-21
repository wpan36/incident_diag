package opsmcp

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wpan36/incident_diag/internal/summary"
)

func TestOkKeepsTheMarkerInsideTheCap(t *testing.T) {
	text, meta := ok(strings.Repeat("a", summary.LimitBytes*2), Meta{})
	got := text.Content[0].(*mcp.TextContent).Text

	// The cap is the size of what leaves this server, not the size of the part
	// before the note saying it was cut.
	if len(got) > summary.LimitBytes {
		t.Errorf("result is %d bytes, over the %d cap", len(got), summary.LimitBytes)
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Error("a truncated result does not say so")
	}
	if !meta.Truncated || meta.OriginalBytes != summary.LimitBytes*2 {
		t.Errorf("truncated=%v original_bytes=%d", meta.Truncated, meta.OriginalBytes)
	}
}

func TestOkLeavesTextThatFitsAlone(t *testing.T) {
	// Exactly at the cap: no room is reserved for a marker that is not coming.
	at := strings.Repeat("a", summary.LimitBytes)
	res, meta := ok(at, Meta{})
	if got := res.Content[0].(*mcp.TextContent).Text; got != at || meta.Truncated {
		t.Errorf("text at exactly the cap was altered: len=%d truncated=%v", len(got), meta.Truncated)
	}
}
