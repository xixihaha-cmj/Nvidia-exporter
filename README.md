# Nvidia-exporter

基于 NVML 的 NVIDIA GPU Prometheus Exporter。

> 代码源自 `mindprince/gonvml` + `BugRoger/nvidia-exporter` 一路 fork（namespace 与指标命名沿用），
> 本仓库在其之上修复了「长期运行会失效」的问题，并补齐了「采集自身健康度」和「卡级健康度」指标。

## 构建

需要 cgo + gcc + NVIDIA 驱动（链接 `-lnvidia-ml`）：

```bash
CGO_ENABLED=1 go build -o nvidia-exporter .
# 国内网络环境：
CGO_ENABLED=1 GOPROXY=https://goproxy.cn,direct GOSUMDB=off go build -o nvidia-exporter .
```

运行机只需要 NVIDIA 驱动（`libnvidia-ml.so.1`），不需要 CUDA / gcc。

## 运行

```bash
/usr/bin/nvidia-exporter -web.listen-address=:9401 \
    -expected-devices-file=/etc/nvidia-exporter/expected_devices \
    -nvml-reinit-interval=5m
```

systemd 单元示例：

```ini
[Service]
ExecStart=/usr/bin/nvidia-exporter -expected-devices-file=/etc/nvidia-exporter/expected_devices -nvml-reinit-interval=5m
Restart=always
RestartSec=5
```

## 命令行参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| -web.listen-address | :9401 | 监听地址 |
| -web.telemetry-path | /metrics | 指标路径 |
| -collect-interval | 5s | 后台采集间隔 |
| -collect-stuck-after | 20s | 采集超过该时长即视为卡死（nvidia_collect_stuck=1） |
| -expected-devices | 0 | 期望 GPU 数量（0 = 不启用） |
| -expected-devices-file | 空 | 期望数量文件（优先于上面的参数） |
| -nvml-reinit-interval | 5m | 定期重新初始化 NVML，自动识别热复位/热插拔的 GPU（0 = 关闭） |

## 指标

**原有（保持兼容）**：`nvidia_device_count`、`nvidia_up`、`nvidia_driver_info`、`nvidia_info`、
`nvidia_utilization_gpu`、`nvidia_utilization_gpu_average`、`nvidia_utilization_memory`、
`nvidia_memory_used`、`nvidia_memory_total`、`nvidia_temperatures`、`nvidia_fanspeed`、
`nvidia_power_usage`、`nvidia_power_usage_average`

**新增 · 采集自身健康度**

| 指标 | 说明 |
| --- | --- |
| nvidia_collect_success | 最近一次采集是否成功（1/0） |
| nvidia_collect_stuck | 1 = 当前采集已超过 -collect-stuck-after 仍未返回 |
| nvidia_collect_timestamp_seconds | 最近一次成功采集的时间戳（做「停更」告警用） |
| nvidia_collect_duration_seconds | 采集耗时 |
| nvidia_collect_timeout_total | 采集被判定为卡死的累计次数 |
| nvidia_collect_errors_total{reason} | 采集错误分类计数 |
| nvidia_nvml_reinit_total{result} | 定期 NVML 重新初始化的次数 |

**新增 · 卡级健康度**

| 指标 | 说明 |
| --- | --- |
| nvidia_gpu_health{index,minor,uuid,name} | 1 = 关键 NVML 调用全部成功；0 = 卡能被枚举但已不可用 |
| nvidia_gpu_error_total{uuid,call} | 失败的 NVML 调用计数（可定位到具体卡与调用名） |
| nvidia_device_expected | 期望 GPU 数量（来自参数或文件） |

## 相对上游的主要改动

1. **修复 fd 泄漏**：NVML 只初始化一次。上游每次抓取都执行 `Initialize/Shutdown`，长时间运行会耗尽文件描述符，
   最终连 HTTP 监听都失败（现场表现为 `could not load NVML library` + `accept4: too many open files`）。
2. **采集改为后台定时 + 缓存**：`/metrics` 只读缓存、不做阻塞调用；驱动/NVML 卡死不再拖垮抓取。
3. **逐卡容错**：单张卡的某个字段读取失败，不再导致整机采集失败。
4. **修正标签**：`nvidia_info` 的 `name`/`uuid` 原来写反；逐卡数值指标统一增加 `gpu_uuid` 标签。
5. **新增健康度信号**：卡能枚举但不能用（`nvidia_gpu_health=0`）、采集停更/卡死、错误分类。
6. **期望卡数 + 定期 NVML 重新初始化**：掉卡可告警；热复位/热插拔的卡最多 5 分钟自动重新枚举。
7. **降噪**：Tesla/GeForce 没有风扇设备的 `Not Supported` 不再每次采集都打日志。

## 许可

沿用上游 LICENSE。