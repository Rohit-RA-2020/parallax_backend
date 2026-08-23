package preview

import "testing"

func TestTimelineFrameCountIsBounded(t *testing.T) {
	tests := []struct {
		duration float64
		want     int
	}{
		{duration: 0, want: 0},
		{duration: 5, want: 4},
		{duration: 60, want: 4},
		{duration: 120, want: 8},
		{duration: 2 * 60 * 60, want: timelineFrameLimit},
	}
	for _, test := range tests {
		if got := timelineFrameCount(test.duration); got != test.want {
			t.Fatalf("timelineFrameCount(%v)=%d want %d", test.duration, got, test.want)
		}
	}
}

func TestTimelineFrameRelIsStable(t *testing.T) {
	if got, want := timelineFrameRel("abc123", 3), ".parallax/previews/abc123-timeline-03.jpg"; got != want {
		t.Fatalf("timelineFrameRel=%q want %q", got, want)
	}
}
