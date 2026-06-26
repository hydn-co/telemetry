package telemetry

import (
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestCounterOnlyDeltaTemporality(t *testing.T) {
	cases := []struct {
		name string
		kind sdkmetric.InstrumentKind
		want metricdata.Temporality
	}{
		{"counter is delta", sdkmetric.InstrumentKindCounter, metricdata.DeltaTemporality},
		{"observable counter is delta", sdkmetric.InstrumentKindObservableCounter, metricdata.DeltaTemporality},
		{"histogram stays cumulative", sdkmetric.InstrumentKindHistogram, metricdata.CumulativeTemporality},
		{"gauge stays cumulative", sdkmetric.InstrumentKindGauge, metricdata.CumulativeTemporality},
		{"observable gauge stays cumulative", sdkmetric.InstrumentKindObservableGauge, metricdata.CumulativeTemporality},
		{"updowncounter stays cumulative", sdkmetric.InstrumentKindUpDownCounter, metricdata.CumulativeTemporality},
		{"observable updowncounter stays cumulative", sdkmetric.InstrumentKindObservableUpDownCounter, metricdata.CumulativeTemporality},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := counterOnlyDeltaTemporality(tc.kind)
			// Assert
			if got != tc.want {
				t.Fatalf("counterOnlyDeltaTemporality(%v) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}
