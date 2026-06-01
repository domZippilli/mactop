package app

import "testing"

// TestSetSensorGaugesPopulatesSurface pins the behavior the headless-mode bug
// violated: setSensorGauges — the single writer shared by the TUI and headless
// paths — must populate the full mactop_* sensor surface from one snapshot.
// It also guards the core_count label parity fix (must be e+p+s, not the
// independently-sourced SystemInfo.CoreCount).
func TestSetSensorGaugesPopulatesSurface(t *testing.T) {
	setSensorGauges(SensorSnapshot{
		CPUUsagePercent: 42,
		CoreUsages:      []float64{10, 20, 30, 40},
		CPU: CPUMetrics{
			CPUW: 1, GPUW: 2, ANEW: 3, DRAMW: 4, GPUSRAMW: 5, SystemW: 6, PackageW: 7,
			CPUTemp: 50, GPUTemp: 55,
			DRAMReadBW: 8, DRAMWriteBW: 9, DRAMBWCombined: 17,
			Fans:        []FanInfo{{ID: 0, Name: "Fan 0", ActualRPM: 1200}},
			TempSensors: []TempSensor{{Key: "TC0P", Name: "CPU", Value: 48.5}},
		},
		GPU:                 GPUMetrics{FreqMHz: 1400, ActivePercent: 73},
		Memory:              MemoryMetrics{Total: 64 << 30, Used: 32 << 30, SwapTotal: 8 << 30, SwapUsed: 1 << 30},
		TBNetInBytesPerSec:  111,
		TBNetOutBytesPerSec: 222,
		RDMAAvailable:       true,
		// CoreCount is deliberately wrong (999) to prove the label uses e+p+s=4.
		System: SystemInfo{Name: "Apple Mtest", CoreCount: 999, ECoreCount: 2, PCoreCount: 2, SCoreCount: 0, GPUCoreCount: 10},
	})

	mfs, err := buildSensorRegistry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	got := map[string]bool{}
	coreCountLabel := ""
	for _, mf := range mfs {
		got[mf.GetName()] = true
		if mf.GetName() == "mactop_system_info" {
			for _, lp := range mf.GetMetric()[0].GetLabel() {
				if lp.GetName() == "core_count" {
					coreCountLabel = lp.GetValue()
				}
			}
		}
	}

	// Every family setSensorGauges owns. networkSpeed/diskIOSpeed/diskIOPS are
	// excluded: they're written by getNetDiskMetrics(), not setSensorGauges.
	want := []string{
		"mactop_cpu_usage_percent",
		"mactop_ecore_usage_percent",
		"mactop_pcore_usage_percent",
		"mactop_score_usage_percent",
		"mactop_gpu_usage_percent",
		"mactop_gpu_freq_mhz",
		"mactop_power_watts",
		"mactop_soc_temp_celsius",
		"mactop_gpu_temp_celsius",
		"mactop_thermal_state",
		"mactop_memory_gb",
		"mactop_dram_bandwidth_gbs",
		"mactop_cpu_core_usage_percent",
		"mactop_system_info",
		"mactop_fan_rpm",
		"mactop_temp_sensor_celsius",
		"mactop_thunderbolt_network_bytes_per_sec",
		"mactop_rdma_available",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("setSensorGauges did not populate %s", name)
		}
	}

	if coreCountLabel != "4" {
		t.Errorf("mactop_system_info core_count = %q, want \"4\" (e+p+s, not CoreCount=999)", coreCountLabel)
	}
}
