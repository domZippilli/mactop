package app

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// buildSensorRegistry returns a registry holding all mactop_* sensor gauges.
// Both the TUI server and the headless server serve from this so the two
// modes expose an identical metric surface.
func buildSensorRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(cpuUsage)
	registry.MustRegister(ecoreUsage)
	registry.MustRegister(pcoreUsage)
	registry.MustRegister(gpuUsage)
	registry.MustRegister(gpuFreqMHz)
	registry.MustRegister(powerUsage)
	registry.MustRegister(socTemp)
	registry.MustRegister(gpuTemp)
	registry.MustRegister(thermalState)
	registry.MustRegister(memoryUsage)
	registry.MustRegister(networkSpeed)
	registry.MustRegister(diskIOSpeed)
	registry.MustRegister(diskIOPS)
	registry.MustRegister(tbNetworkSpeed)
	registry.MustRegister(rdmaAvailable)
	registry.MustRegister(scoreUsage)
	registry.MustRegister(dramBandwidth)
	registry.MustRegister(cpuCoreUsage)
	registry.MustRegister(systemInfoGauge)
	registry.MustRegister(fanRPM)
	registry.MustRegister(tempSensorGauge)
	return registry
}

func startPrometheusServer(port string) {
	handler := promhttp.HandlerFor(buildSensorRegistry(), promhttp.HandlerOpts{})

	http.Handle("/metrics", handler)
	go func() {
		err := http.ListenAndServe(normalizeListenAddr(port), nil)
		if err != nil {
			stderrLogger.Printf("Failed to start Prometheus metrics server: %v\n", err)
		}
	}()
}

// normalizeListenAddr accepts either a bare port ("2112") or a full listen
// address (":2112", "127.0.0.1:2112") and returns a valid ListenAndServe addr.
func normalizeListenAddr(port string) string {
	if strings.Contains(port, ":") {
		return port
	}
	return ":" + port
}

// SensorSnapshot carries one sample's worth of sensor data. It is the single
// input to setSensorGauges, so the TUI and headless paths can produce an
// identical mactop_* metric surface from the same code.
type SensorSnapshot struct {
	CPUUsagePercent     float64
	CoreUsages          []float64
	CPU                 CPUMetrics // power, temps, DRAM bandwidth, fans, temp sensors
	GPU                 GPUMetrics
	Memory              MemoryMetrics
	TBNetInBytesPerSec  float64
	TBNetOutBytesPerSec float64
	RDMAAvailable       bool
	System              SystemInfo
}

// setSensorGauges is the single writer for every mactop_* sensor gauge. Both
// the TUI render loop (finalizeCPUUI) and the headless sample loop build a
// SensorSnapshot and call this, so adding or renaming a metric is a one-place
// change the compiler can check. The networkSpeed/diskIOSpeed/diskIOPS gauges
// are intentionally excluded: they already have a single writer inside
// getNetDiskMetrics() that both modes call, so duplicating them here would
// reintroduce the very split this function exists to remove.
func setSensorGauges(s SensorSnapshot) {
	cpuUsage.Set(s.CPUUsagePercent)

	ecoreAvg, pcoreAvg, scoreAvg := clusterUsageAverages(s.CoreUsages, s.System)
	ecoreUsage.Set(ecoreAvg)
	pcoreUsage.Set(pcoreAvg)
	scoreUsage.Set(scoreAvg)

	powerUsage.With(prometheus.Labels{"component": "cpu"}).Set(s.CPU.CPUW)
	powerUsage.With(prometheus.Labels{"component": "gpu"}).Set(s.CPU.GPUW)
	powerUsage.With(prometheus.Labels{"component": "ane"}).Set(s.CPU.ANEW)
	powerUsage.With(prometheus.Labels{"component": "dram"}).Set(s.CPU.DRAMW)
	powerUsage.With(prometheus.Labels{"component": "gpu_sram"}).Set(s.CPU.GPUSRAMW)
	powerUsage.With(prometheus.Labels{"component": "system"}).Set(s.CPU.SystemW)
	powerUsage.With(prometheus.Labels{"component": "total"}).Set(s.CPU.PackageW)

	socTemp.Set(s.CPU.CPUTemp)
	gpuTemp.Set(s.CPU.GPUTemp)

	thermalStateNum := 0
	switch getThermalStateLevel() {
	case thermalStateFair:
		thermalStateNum = 1
	case thermalStateSerious:
		thermalStateNum = 2
	case thermalStateCritical:
		thermalStateNum = 3
	}
	thermalState.Set(float64(thermalStateNum))

	dramBandwidth.With(prometheus.Labels{"direction": "read"}).Set(s.CPU.DRAMReadBW)
	dramBandwidth.With(prometheus.Labels{"direction": "write"}).Set(s.CPU.DRAMWriteBW)
	dramBandwidth.With(prometheus.Labels{"direction": "combined"}).Set(s.CPU.DRAMBWCombined)

	memoryUsage.With(prometheus.Labels{"type": "used"}).Set(float64(s.Memory.Used) / 1024 / 1024 / 1024)
	memoryUsage.With(prometheus.Labels{"type": "total"}).Set(float64(s.Memory.Total) / 1024 / 1024 / 1024)
	memoryUsage.With(prometheus.Labels{"type": "swap_used"}).Set(float64(s.Memory.SwapUsed) / 1024 / 1024 / 1024)
	memoryUsage.With(prometheus.Labels{"type": "swap_total"}).Set(float64(s.Memory.SwapTotal) / 1024 / 1024 / 1024)

	eCoreCount := s.System.ECoreCount
	pEnd := eCoreCount + s.System.PCoreCount
	for i, usage := range s.CoreUsages {
		coreType := "s"
		if i < eCoreCount {
			coreType = "e"
		} else if i < pEnd {
			coreType = "p"
		}
		cpuCoreUsage.With(prometheus.Labels{"core": fmt.Sprintf("%d", i), "type": coreType}).Set(usage)
	}

	gpuUsage.Set(s.GPU.ActivePercent)
	gpuFreqMHz.Set(float64(s.GPU.FreqMHz))

	tbNetworkSpeed.With(prometheus.Labels{"direction": "download"}).Set(s.TBNetInBytesPerSec)
	tbNetworkSpeed.With(prometheus.Labels{"direction": "upload"}).Set(s.TBNetOutBytesPerSec)

	if s.RDMAAvailable {
		rdmaAvailable.Set(1)
	} else {
		rdmaAvailable.Set(0)
	}

	updatePrometheusSensors(s.CPU.Fans, s.CPU.TempSensors)

	systemInfoGauge.With(prometheus.Labels{
		"model":          s.System.Name,
		"core_count":     fmt.Sprintf("%d", s.System.ECoreCount+s.System.PCoreCount+s.System.SCoreCount),
		"e_core_count":   fmt.Sprintf("%d", s.System.ECoreCount),
		"p_core_count":   fmt.Sprintf("%d", s.System.PCoreCount),
		"s_core_count":   fmt.Sprintf("%d", s.System.SCoreCount),
		"gpu_core_count": fmt.Sprintf("%d", s.System.GPUCoreCount),
	}).Set(1)
}

// clusterUsageAverages computes E/P/S cluster usage averages from the flat
// per-core list, using SystemInfo core counts. Shared by both modes so the
// TUI and headless derive ecore/pcore/score the same way.
func clusterUsageAverages(coreUsages []float64, info SystemInfo) (ecoreAvg, pcoreAvg, scoreAvg float64) {
	if info.ECoreCount > 0 && len(coreUsages) >= info.ECoreCount {
		for i := 0; i < info.ECoreCount; i++ {
			ecoreAvg += coreUsages[i]
		}
		ecoreAvg /= float64(info.ECoreCount)
	}
	pStart := info.ECoreCount
	pEnd := pStart + info.PCoreCount
	if info.PCoreCount > 0 && len(coreUsages) >= pEnd {
		for i := pStart; i < pEnd; i++ {
			pcoreAvg += coreUsages[i]
		}
		pcoreAvg /= float64(info.PCoreCount)
	}
	sStart := pEnd
	sEnd := sStart + info.SCoreCount
	if info.SCoreCount > 0 && len(coreUsages) >= sEnd {
		for i := sStart; i < sEnd; i++ {
			scoreAvg += coreUsages[i]
		}
		scoreAvg /= float64(info.SCoreCount)
	}
	return ecoreAvg, pcoreAvg, scoreAvg
}

func GetCPUPercentages() ([]float64, error) {
	currentTimes, err := GetCPUUsage()
	if err != nil {
		return nil, err
	}
	if firstRun {
		lastCPUTimes = currentTimes
		firstRun = false
		return make([]float64, len(currentTimes)), nil
	}
	percentages := make([]float64, len(currentTimes))
	for i := range currentTimes {
		totalDelta := (currentTimes[i].User - lastCPUTimes[i].User) +
			(currentTimes[i].System - lastCPUTimes[i].System) +
			(currentTimes[i].Idle - lastCPUTimes[i].Idle) +
			(currentTimes[i].Nice - lastCPUTimes[i].Nice)

		activeDelta := (currentTimes[i].User - lastCPUTimes[i].User) +
			(currentTimes[i].System - lastCPUTimes[i].System) +
			(currentTimes[i].Nice - lastCPUTimes[i].Nice)

		if totalDelta > 0 {
			percentages[i] = (activeDelta / totalDelta) * 100.0
		}
		if percentages[i] < 0 {
			percentages[i] = 0
		} else if percentages[i] > 100 {
			percentages[i] = 100
		}
	}
	lastCPUTimes = currentTimes
	return percentages, nil
}

func getNetDiskMetrics() NetDiskMetrics {
	var metrics NetDiskMetrics

	netDiskMutex.Lock()
	defer netDiskMutex.Unlock()

	now := time.Now()
	elapsed := now.Sub(lastNetDiskTime).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}

	// Native Network Metrics
	netMap, err := GetNativeNetworkMetrics()
	if err == nil {
		var totalNet NativeNetMetric
		for _, iface := range netMap {
			totalNet.BytesRecv += iface.BytesRecv
			totalNet.BytesSent += iface.BytesSent
			totalNet.PacketsRecv += iface.PacketsRecv
			totalNet.PacketsSent += iface.PacketsSent
		}

		if lastNetDiskTime.IsZero() {
			lastNetStats = totalNet
		} else {
			metrics.InBytesPerSec = float64(totalNet.BytesRecv-lastNetStats.BytesRecv) / elapsed
			metrics.OutBytesPerSec = float64(totalNet.BytesSent-lastNetStats.BytesSent) / elapsed
			metrics.InPacketsPerSec = float64(totalNet.PacketsRecv-lastNetStats.PacketsRecv) / elapsed
			metrics.OutPacketsPerSec = float64(totalNet.PacketsSent-lastNetStats.PacketsSent) / elapsed
		}
		lastNetStats = totalNet
	}

	// Native Disk Metrics
	diskMap, err := GetNativeDiskMetrics()
	if err == nil {
		var totalDisk NativeDiskMetric
		for _, d := range diskMap {
			totalDisk.ReadBytes += d.ReadBytes
			totalDisk.WriteBytes += d.WriteBytes
			totalDisk.ReadOps += d.ReadOps
			totalDisk.WriteOps += d.WriteOps
		}

		if !lastNetDiskTime.IsZero() {
			metrics.ReadKBytesPerSec = float64(totalDisk.ReadBytes-lastDiskStats.ReadBytes) / elapsed / 1024
			metrics.WriteKBytesPerSec = float64(totalDisk.WriteBytes-lastDiskStats.WriteBytes) / elapsed / 1024
			metrics.ReadOpsPerSec = float64(totalDisk.ReadOps-lastDiskStats.ReadOps) / elapsed
			metrics.WriteOpsPerSec = float64(totalDisk.WriteOps-lastDiskStats.WriteOps) / elapsed
		}
		lastDiskStats = totalDisk
	}

	networkSpeed.With(prometheus.Labels{"direction": "upload"}).Set(metrics.OutBytesPerSec)
	networkSpeed.With(prometheus.Labels{"direction": "download"}).Set(metrics.InBytesPerSec)
	diskIOSpeed.With(prometheus.Labels{"operation": "read"}).Set(metrics.ReadKBytesPerSec * 1024)
	diskIOSpeed.With(prometheus.Labels{"operation": "write"}).Set(metrics.WriteKBytesPerSec * 1024)
	diskIOPS.With(prometheus.Labels{"operation": "read"}).Set(metrics.ReadOpsPerSec)
	diskIOPS.With(prometheus.Labels{"operation": "write"}).Set(metrics.WriteOpsPerSec)

	lastNetDiskTime = now
	return metrics
}

func collectNetDiskMetrics(done chan struct{}, netdiskMetricsChan chan NetDiskMetrics) {
	for {
		start := time.Now()

		netdiskMetrics := getNetDiskMetrics()
		select {
		case <-done:
			return
		case netdiskMetricsChan <- netdiskMetrics:
		default:
		}

		elapsed := time.Since(start)
		sleepTime := time.Duration(updateInterval)*time.Millisecond - elapsed
		if sleepTime > 0 {
			select {
			case <-time.After(sleepTime):
			case <-interruptChan:
			}
		}
	}
}

// dispatchMetrics sends metrics to channels without blocking, checking done for exit.
func dispatchMetrics(done chan struct{}, cpuCh chan CPUMetrics, gpuCh chan GPUMetrics,
	tbCh chan []ThunderboltNetStats, triggerCh chan struct{},
	cpu CPUMetrics, gpu GPUMetrics, tb []ThunderboltNetStats) bool {
	select {
	case <-done:
		return true
	case cpuCh <- cpu:
	default:
	}
	select {
	case gpuCh <- gpu:
	default:
	}
	select {
	case tbCh <- tb:
	default:
	}
	select {
	case triggerCh <- struct{}{}:
	default:
	}
	return false
}

func collectMetrics(done chan struct{}, cpumetricsChan chan CPUMetrics, gpumetricsChan chan GPUMetrics, tbNetStatsChan chan []ThunderboltNetStats, triggerProcessCollectionChan chan struct{}) {
	// Pre-calculate static info
	sysInfo := getSOCInfo()
	maxGPUFreq := GetMaxGPUFrequency()
	var maxFP32TFLOPs float64
	if maxGPUFreq > 0 && sysInfo.GPUCoreCount > 0 {
		maxFP32TFLOPs = float64(sysInfo.GPUCoreCount) * float64(maxGPUFreq) * 0.000256
	}

	for {
		start := time.Now()

		sampleDuration := updateInterval
		if sampleDuration < 100 {
			sampleDuration = 100
		}

		m := sampleSocMetrics(sampleDuration / 2)

		thermalStr, throttled := getThermalStateString()
		rdmaStat := CheckRDMAAvailable().Status

		componentSum := m.TotalPower
		totalPower := componentSum
		systemResidual := 0.0

		if m.SystemPower > componentSum {
			totalPower = m.SystemPower
			systemResidual = m.SystemPower - componentSum
		}

		coreUsages, _ := GetCPUPercentages()
		avgUsage := 0.0
		if len(coreUsages) > 0 {
			for _, p := range coreUsages {
				avgUsage += p
			}
			avgUsage /= float64(len(coreUsages))
		}

		cpuMetrics := CPUMetrics{
			CPUW:            m.CPUPower,
			GPUW:            m.GPUPower,
			ANEW:            m.ANEPower,
			DRAMW:           m.DRAMPower,
			GPUSRAMW:        m.GPUSRAMPower,
			SystemW:         systemResidual,
			PackageW:        totalPower,
			Throttled:       throttled,
			CPUTemp:         float64(m.CPUTemp),
			GPUTemp:         float64(m.GPUTemp),
			EClusterActive:  int(m.EClusterActive),
			PClusterActive:  int(m.PClusterActive),
			EClusterFreqMHz: int(m.EClusterFreqMHz),
			PClusterFreqMHz: int(m.PClusterFreqMHz),
			SClusterActive:  int(m.SClusterActive),
			SClusterFreqMHz: int(m.SClusterFreqMHz),
			DRAMReadBW:      m.DRAMReadBW,
			DRAMWriteBW:     m.DRAMWriteBW,
			DRAMBWCombined:  m.DRAMBWCombined,
			Fans:            m.Fans,
			TempSensors:     m.TempSensors,
			CoreUsages:      coreUsages,
			AvgUsage:        avgUsage,
		}

		gpuMetrics := GPUMetrics{
			FreqMHz:       int(m.GPUFreqMHz),
			ActivePercent: m.GPUActive,
			Power:         m.GPUPower + m.GPUSRAMPower,
			Temp:          m.GPUTemp,
		}

		// Fan/temp-sensor gauges are now set via setSensorGauges (called from
		// finalizeCPUUI with this sample's Fans/TempSensors), not here.

		if dispatchMetrics(done, cpumetricsChan, gpumetricsChan, tbNetStatsChan, triggerProcessCollectionChan, cpuMetrics, gpuMetrics, GetThunderboltNetStats()) {
			return
		}

		// Push to menubar worker — snapshot net metrics under lock to avoid race
		if menubar {
			renderMutex.Lock()
			nd := lastNetDiskMetrics
			renderMutex.Unlock()
			pushMenuBarMetricsToWorker(m, cpuMetrics, gpuMetrics, nd, sysInfo, maxFP32TFLOPs, cpuMetrics.AvgUsage, thermalStr, rdmaStat)
		}

		// Push to overlay worker
		if overlay {
			renderMutex.Lock()
			nd := lastNetDiskMetrics
			renderMutex.Unlock()
			pushOverlayMetrics(m, cpuMetrics, gpuMetrics, nd, sysInfo, maxFP32TFLOPs, cpuMetrics.AvgUsage, thermalStr, rdmaStat)
		}

		elapsed := time.Since(start)
		sleepTime := time.Duration(updateInterval)*time.Millisecond - elapsed
		if sleepTime > 0 {
			select {
			case <-time.After(sleepTime):
			case <-interruptChan:
			}
		}
	}
}

func updatePrometheusSensors(fans []FanInfo, sensors []TempSensor) {
	for _, fan := range fans {
		fanRPM.With(prometheus.Labels{"fan_id": fmt.Sprintf("%d", fan.ID), "fan_name": fan.Name}).Set(float64(fan.ActualRPM))
	}
	for _, sensor := range sensors {
		tempSensorGauge.With(prometheus.Labels{"key": sensor.Key, "name": sensor.Name}).Set(sensor.Value)
	}
}

func collectProcessMetrics(done chan struct{}, processMetricsChan chan []ProcessMetrics, triggerChan chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-triggerChan:
			renderMutex.Lock()
			sysPct := lastGPUMetrics.ActivePercent
			renderMutex.Unlock()

			if processes, err := getProcessList(sysPct); err == nil {
				processMetricsChan <- processes
			} else {
				stderrLogger.Printf("Error getting process list: %v\n", err)
			}
		}
	}
}

func getMemoryMetrics() MemoryMetrics {
	native, err := GetNativeMemoryMetrics()
	if err != nil {
		stderrLogger.Printf("Error getting native memory metrics: %v\n", err)
		return MemoryMetrics{}
	}
	return MemoryMetrics{
		Total:     native.Total,
		Used:      native.Used,
		Available: native.Available,
		SwapTotal: native.SwapTotal,
		SwapUsed:  native.SwapUsed,
	}
}
