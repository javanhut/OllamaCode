package api

import "testing"

func TestParamsB(t *testing.T) {
	cases := []struct {
		size string
		info map[string]any
		want float64
	}{
		{"12.4B", nil, 12.4},
		{"756b", nil, 756},
		{"1t", nil, 1000},
		{"270m", nil, 0.27},
		{"", nil, 0},
		{"weird", nil, 0},
		{"7B", map[string]any{"general.parameter_count": float64(35951822704)}, 35.951822704}, // exact count wins
	}
	for _, c := range cases {
		r := ShowModelResponse{ModelInfo: c.info}
		r.Details.ParameterSize = c.size
		got := r.ParamsB()
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("ParamsB(%q, %v) = %v, want %v", c.size, c.info, got, c.want)
		}
	}
}
