package tracing

import (
	"math"
	"testing"
)

func TestApdex(t *testing.T) {
	const T int64 = 500 // satisfied <=500ms, tolerated <=2000ms, frustrated >2000ms

	tests := []struct {
		name      string
		durations []int64
		t         int64
		want      float64
	}{
		{
			name:      "empty input returns zero",
			durations: nil,
			t:         T,
			want:      0,
		},
		{
			name:      "all satisfied",
			durations: []int64{10, 100, 200, 499, 500},
			t:         T,
			want:      1.0,
		},
		{
			name:      "all frustrated",
			durations: []int64{2001, 5000, 10000},
			t:         T,
			want:      0.0,
		},
		{
			name:      "all tolerated",
			durations: []int64{501, 1000, 1500, 2000},
			t:         T,
			want:      0.5,
		},
		{
			name:      "mixed: 2 satisfied, 1 tolerated, 1 frustrated",
			durations: []int64{100, 400, 1500, 5000},
			t:         T,
			// (2 + 1/2) / 4 = 0.625
			want: 0.625,
		},
		{
			name:      "boundary at T is satisfied",
			durations: []int64{500},
			t:         T,
			want:      1.0,
		},
		{
			name:      "boundary at 4T is tolerated",
			durations: []int64{2000},
			t:         T,
			want:      0.5,
		},
		{
			name:      "just past 4T is frustrated",
			durations: []int64{2001},
			t:         T,
			want:      0.0,
		},
		{
			name:      "zero threshold returns zero",
			durations: []int64{1, 2, 3},
			t:         0,
			want:      0,
		},
		{
			name:      "negative threshold returns zero",
			durations: []int64{1, 2, 3},
			t:         -10,
			want:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apdex(tt.durations, tt.t)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("apdex(%v, %d) = %v, want %v", tt.durations, tt.t, got, tt.want)
			}
		})
	}
}
