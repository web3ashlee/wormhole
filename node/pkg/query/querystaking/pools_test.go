package querystaking

import (
	"testing"

	"github.com/certusone/wormhole/node/pkg/query/queryratelimit"
	"github.com/holiman/uint256"
)

// TestCalculateRates tests the rate calculation logic with conversion tables
func TestCalculateRates(t *testing.T) {
	tests := []struct {
		name       string
		stake      *uint256.Int
		conversion string
		want       queryratelimit.Rule
	}{
		// Zero/nil stake tests
		{
			name:       "nil stake",
			stake:      nil,
			conversion: "rate:100,tranche:1000",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 0},
		},
		{
			name:       "zero stake",
			stake:      uint256.NewInt(0),
			conversion: "rate:100,tranche:1000",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 0},
		},

		// Invalid conversion entry tests
		{
			name:       "empty conversion entry",
			stake:      uint256.NewInt(10000),
			conversion: "",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 0},
		},
		{
			name:       "invalid conversion format",
			stake:      uint256.NewInt(10000),
			conversion: "invalid",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 0},
		},

		// Single tranche tests
		{
			name:       "stake below minimum tranche",
			stake:      uint256.NewInt(4999),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 0},
		},
		{
			name:       "stake at minimum tranche",
			stake:      uint256.NewInt(5000),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 10}, // (5000/5000)*10 = 10 QPM
		},
		{
			name:       "stake above minimum tranche",
			stake:      uint256.NewInt(10000),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 20}, // (10000/5000)*10 = 20 QPM
		},

		// Multiple tranche tests
		{
			name:       "qualifies for first tranche only",
			stake:      uint256.NewInt(10000),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 20}, // (10000/5000)*10 = 20 QPM
		},
		{
			name:       "qualifies for higher tranche",
			stake:      uint256.NewInt(100000),
			conversion: "rate:100,tranche:50000",
			want:       queryratelimit.Rule{MaxPerSecond: 3, MaxPerMinute: 200}, // (100000/50000)*100 = 200 QPM, 200/60 = 3 QPS
		},

		// QPM to QPS conversion tests
		{
			name:       "QPM less than 60 - no QPS",
			stake:      uint256.NewInt(25000),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 50}, // (25000/5000)*10 = 50 QPM, no QPS
		},
		{
			name:       "QPM equals 60 - 1 QPS",
			stake:      uint256.NewInt(30000),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 1, MaxPerMinute: 60}, // (30000/5000)*10 = 60 QPM, 60/60 = 1 QPS
		},
		{
			name:       "QPM equals 120 - 2 QPS",
			stake:      uint256.NewInt(60000),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 2, MaxPerMinute: 120}, // (60000/5000)*10 = 120 QPM, 120/60 = 2 QPS
		},
		{
			name:       "QPM with truncation in division",
			stake:      uint256.NewInt(59500),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 1, MaxPerMinute: 110}, // (59500/5000)*10 = 11*10 = 110 QPM (59500/5000 truncates to 11), 110/60 = 1 QPS
		},
		{
			name:       "high QPM - 600 QPM becomes 10 QPS",
			stake:      uint256.NewInt(300000),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 10, MaxPerMinute: 600}, // (300000/5000)*10 = 600 QPM, 600/60 = 10 QPS
		},

		// Edge case tests
		{
			name:       "exact tranche boundary",
			stake:      uint256.NewInt(50000),
			conversion: "rate:100,tranche:50000",
			want:       queryratelimit.Rule{MaxPerSecond: 1, MaxPerMinute: 100}, // (50000/50000)*100 = 100 QPM, 100/60 = 1 QPS
		},
		{
			name:       "one less than tranche boundary",
			stake:      uint256.NewInt(49999),
			conversion: "rate:10,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 1, MaxPerMinute: 90}, // (49999/5000)*10 = 9*10 = 90 QPM (49999/5000 = 9, truncated), 90/60 = 1 QPS
		},
		{
			name:       "large stake amount",
			stake:      uint256.NewInt(1000000),
			conversion: "rate:100,tranche:50000",
			want:       queryratelimit.Rule{MaxPerSecond: 33, MaxPerMinute: 2000}, // (1000000/50000)*100 = 2000 QPM, 2000/60 = 33 QPS
		},

		// Zero rate/tranche tests
		{
			name:       "zero rate in tranche",
			stake:      uint256.NewInt(10000),
			conversion: "rate:0,tranche:5000",
			want:       queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 0}, // (10000/5000)*0 = 0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversionEntry := toBytes32(tt.conversion)
			tranches, err := ParseConversionTranches(conversionEntry)
			// If parse fails, CalculateRates should return zero (like it did before)
			if err != nil {
				tranches = []ConversionTranche{}
			}
			// Use decimals=0 so stake values are treated as token units (no conversion)
			got := CalculateRates(tt.stake, tranches, 0)

			if got.MaxPerSecond != tt.want.MaxPerSecond {
				t.Errorf("CalculateRates() MaxPerSecond = %d, want %d", got.MaxPerSecond, tt.want.MaxPerSecond)
			}
			if got.MaxPerMinute != tt.want.MaxPerMinute {
				t.Errorf("CalculateRates() MaxPerMinute = %d, want %d", got.MaxPerMinute, tt.want.MaxPerMinute)
			}
		})
	}
}

// TestCalculateRates_IntegerDivision tests integer division behavior
func TestCalculateRates_IntegerDivision(t *testing.T) {
	tests := []struct {
		name       string
		stake      uint64
		conversion string
		wantQPM    int
		wantQPS    int
	}{
		{
			name:       "division with no remainder",
			stake:      10000,
			conversion: "rate:10,tranche:5000",
			wantQPM:    20, // (10000/5000)*10 = 2*10 = 20
			wantQPS:    0,
		},
		{
			name:       "division truncates remainder",
			stake:      12500,
			conversion: "rate:10,tranche:5000",
			wantQPM:    20, // (12500/5000)*10 = 2*10 = 20 (12500/5000 = 2.5, truncated to 2)
			wantQPS:    0,
		},
		{
			name:       "QPS truncates fractional result",
			stake:      65000,
			conversion: "rate:10,tranche:5000",
			wantQPM:    130, // (65000/5000)*10 = 13*10 = 130
			wantQPS:    2,   // 130/60 = 2.166..., truncated to 2
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stake := uint256.NewInt(tt.stake)
			conversionEntry := toBytes32(tt.conversion)
			tranches, err := ParseConversionTranches(conversionEntry)
			if err != nil {
				t.Fatalf("ParseConversionTranches() error = %v", err)
			}
			// Use decimals=0 so stake values are treated as token units (no conversion)
			got := CalculateRates(stake, tranches, 0)

			if got.MaxPerMinute != tt.wantQPM {
				t.Errorf("CalculateRates() QPM = %d, want %d", got.MaxPerMinute, tt.wantQPM)
			}
			if got.MaxPerSecond != tt.wantQPS {
				t.Errorf("CalculateRates() QPS = %d, want %d", got.MaxPerSecond, tt.wantQPS)
			}
		})
	}
}
