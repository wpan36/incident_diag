package wire

import (
	"encoding/json"
	"testing"
	"time"
)

// --- timestamps -----------------------------------------------------------

func TestTimeRendersFixedMicrosecondPrecision(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			// The case Go's default RFC3339Nano gets wrong: it would trim the
			// zeros and emit "2026-09-20T10:11:12Z".
			name: "a whole second still shows six digits",
			in:   time.Date(2026, 9, 20, 10, 11, 12, 0, time.UTC),
			want: `"2026-09-20T10:11:12.000000Z"`,
		},
		{
			name: "microseconds are kept",
			in:   time.Date(2026, 9, 20, 10, 11, 12, 345678000, time.UTC),
			want: `"2026-09-20T10:11:12.345678Z"`,
		},
		{
			name: "a non-UTC instant is converted, not relabelled",
			in:   time.Date(2026, 9, 20, 10, 11, 12, 0, time.FixedZone("UTC+2", 2*60*60)),
			want: `"2026-09-20T08:11:12.000000Z"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(NewTime(tt.in))
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNullTimeMarshalsAsNull(t *testing.T) {
	body, err := json.Marshal(struct {
		At *Time `json:"at"`
	}{At: NullTime(nil)})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if string(body) != `{"at":null}` {
		t.Fatalf("got %s, want an explicit null", body)
	}
}
