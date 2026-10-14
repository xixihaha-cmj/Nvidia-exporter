package main

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mindprince/gonvml"
)

var (
	averageDuration = 10 * time.Second
)

// Metrics 是一次完整采集的结果。
type Metrics struct {
	Version string
	Devices []*Device
}

// Device 是单张卡的采集结果。
//
// Health = 0 表示这张卡还能被 NVML 枚举，但有关键调用失败
// （也就是「nvidia-smi 里还能看到这张卡，但已经不能用」的那种卡）。
type Device struct {
	Index                 string
	MinorNumber           string
	Name                  string
	UUID                  string
	Temperature           float64
	PowerUsage            float64
	PowerUsageAverage     float64
	FanSpeed              float64
	MemoryTotal           float64
	MemoryUsed            float64
	UtilizationMemory     float64
	UtilizationGPU        float64
	UtilizationGPUAverage float64

	Health      float64
	FailedCalls []string
}

func (d *Device) fail(call string) {
	d.Health = 0
	d.FailedCalls = append(d.FailedCalls, call)
}

func isNotSupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Not Supported")
}

// NVML 初始化必须只做一次。
//
// 旧实现在每次抓取时都 gonvml.Initialize()/Shutdown()，而 gonvml 的动态加载版本
// 每次 Initialize 都会 dlopen 一次 libnvidia-ml.so.1 且不会 dlclose，于是每抓一次
// 泄漏一个 fd。按 15~40 秒一次抓取，几周就能把进程 fd 上限（现场 524288）耗光，
// 之后既加载不了 NVML（日志刷 "could not load NVML library"），连 HTTP listener
// 的 accept4 都会失败 —— 一区 cn10 / cn36 就是这个故障。
var (
	nvmlMu       sync.Mutex
	nvmlReady    bool
	nvmlErr      error
	nvmlAttempts int
)

const nvmlMaxInitAttempts = 20

// nvmlInit 只初始化一次（失败后有限次重试）。
// 绝不在每次抓取时反复 Initialize —— 动态加载版本会因此每次 dlopen 泄漏一个 fd。
func nvmlInit() error {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	if nvmlReady {
		return nil
	}
	if nvmlErr != nil && nvmlAttempts >= nvmlMaxInitAttempts {
		return nvmlErr
	}
	nvmlAttempts++
	nvmlErr = gonvml.Initialize()
	if nvmlErr == nil {
		nvmlReady = true
	}
	return nvmlErr
}

// nvmlReinit 周期性重新初始化 NVML。
// 用途：GPU 被热复位/热插拔（例如对坏卡做 PCI 复位）后，NVML 不会自动感知，
// 必须 Shutdown + Initialize 才能重新枚举。由 -nvml-reinit-interval 控制（默认 5 分钟）。
func nvmlReinit() error {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	if nvmlReady {
		_ = gonvml.Shutdown()
	}
	nvmlReady = false
	nvmlErr = nil
	nvmlAttempts = 0
	nvmlErr = gonvml.Initialize()
	if nvmlErr == nil {
		nvmlReady = true
	}
	return nvmlErr
}

// collectMetrics 逐卡采集。
//
// 与旧实现最大的区别：单张卡的某个字段读失败不会让整台机器的采集失败
// （旧实现是任一字段报错就 return nil, err，一张坏卡会把整机指标打断）。
func collectMetrics() (*Metrics, error) {
	if err := nvmlInit(); err != nil {
		return nil, err
	}

	version, err := gonvml.SystemDriverVersion()
	if err != nil {
		return nil, err
	}

	metrics := &Metrics{Version: version}

	numDevices, err := gonvml.DeviceCount()
	if err != nil {
		return nil, err
	}

	for index := 0; index < int(numDevices); index++ {
		d := &Device{
			Index:  strconv.Itoa(index),
			Health: 1,
		}

		device, err := gonvml.DeviceHandleByIndex(uint(index))
		if err != nil {
			d.UUID = "unknown-" + d.Index
			d.Name = "unknown"
			d.MinorNumber = d.Index
			d.fail("nvmlDeviceGetHandleByIndex")
			metrics.Devices = append(metrics.Devices, d)
			continue
		}

		if v, err := device.UUID(); err != nil {
			d.fail("nvmlDeviceGetUUID")
		} else {
			d.UUID = v
		}

		if v, err := device.Name(); err != nil {
			d.fail("nvmlDeviceGetName")
		} else {
			d.Name = v
		}

		if v, err := device.MinorNumber(); err != nil {
			d.fail("nvmlDeviceGetMinorNumber")
		} else {
			d.MinorNumber = strconv.Itoa(int(v))
		}

		if v, err := device.Temperature(); err != nil {
			d.fail("nvmlDeviceGetTemperature")
		} else {
			d.Temperature = float64(v)
		}

		if v, err := device.PowerUsage(); err != nil {
			d.fail("nvmlDeviceGetPowerUsage")
		} else {
			d.PowerUsage = float64(v)
		}

		if v, err := device.AveragePowerUsage(averageDuration); err != nil {
			d.fail("nvmlDeviceGetPowerUsageAverage")
		} else {
			d.PowerUsageAverage = float64(v)
		}

		// Tesla 卡（T4 / P100）没有风扇，返回 Not Supported 属于正常情况：
		// 既不算这张卡不健康，也不打日志（旧实现每次采集都刷两行）。
		if v, err := device.FanSpeed(); err != nil {
			if !isNotSupported(err) {
				d.fail("nvmlDeviceGetFanSpeed")
			}
		} else {
			d.FanSpeed = float64(v)
		}

		if total, used, err := device.MemoryInfo(); err != nil {
			d.fail("nvmlDeviceGetMemoryInfo")
		} else {
			d.MemoryTotal = float64(total)
			d.MemoryUsed = float64(used)
		}

		if utilizationGPU, utilizationMemory, err := device.UtilizationRates(); err != nil {
			d.fail("nvmlDeviceGetUtilizationRates")
		} else {
			d.UtilizationGPU = float64(utilizationGPU)
			d.UtilizationMemory = float64(utilizationMemory)
		}

		if v, err := device.AverageGPUUtilization(averageDuration); err != nil {
			d.fail("nvmlDeviceGetUtilizationRatesAverage")
		} else {
			d.UtilizationGPUAverage = float64(v)
		}

		if d.UUID == "" {
			d.UUID = "unknown-" + d.Index
		}
		if d.Name == "" {
			d.Name = "unknown"
		}
		if d.MinorNumber == "" {
			d.MinorNumber = d.Index
		}

		metrics.Devices = append(metrics.Devices, d)
	}

	return metrics, nil
}
