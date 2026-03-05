package perf

import (
	"encoding/json"
	gosync "sync"
	"time"

	"github.com/baetyl/baetyl-go/v2/log"
)

// Stage 定义 TopicDownside proxy data 流程中的各个阶段
const (
	// === 下行阶段 (Cloud -> Edge Target) ===
	// 阶段1: wsLink.receiving() 通过 conn.NextReader() 读取 WebSocket 帧并 JSON 解码为 *v1.Message
	StageWsReceiveDecode = "ws_receive_decode"
	// 阶段2: wsLink.receiving() 将消息投递到 msgCh
	StageWsMsgChDeliver = "ws_msgch_deliver"
	// 阶段3: sync.dispatch() 将消息发布到 TopicDownside pubsub
	StageSyncDispatch = "sync_dispatch_to_downside"
	// 阶段4: engine handlerDownside.OnMessage() 处理消息路由
	StageEngineDownsideRoute = "engine_downside_route"
	// 阶段5a (仅首条CMD): engine.proxy() 创建 chain 并建立 TCP 连接
	StageProxyChainCreate = "proxy_chain_create"
	// 阶段5b (后续Data): engine 将 MessageData 发布到 chain 的 downside topic
	StageEngineToChainDownside = "engine_to_chain_downside"
	// 阶段6: chainHandler.OnMessage() 反序列化并写入 pipe.InWriter
	StageChainHandlerWrite = "chain_handler_write_pipe"
	// 阶段7: proxy.go 从 pipe.InReader 读取并写入 TCP conn (发送到目标服务)
	StageProxyWriteTarget = "proxy_write_to_target"

	// === 上行阶段 (Edge Target -> Cloud) ===
	// 阶段8: proxy.go 从 TCP conn 读取目标服务响应并写入 pipe.OutWriter
	StageProxyReadAndWritePipe = "proxy_read_target_write_pipe"
	// 阶段9: chainReading() 从 pipe.OutReader 读取并发布到 TopicUpside
	StageChainReadingPublish = "chain_reading_publish_upside"
	// 阶段10: sync upside handler 通过 wsLink.Send() (conn.WriteJSON) 发送到云端
	StageWsSend = "ws_send_to_cloud"
)

// PerfRecord 记录单次请求的各阶段耗时
type PerfRecord struct {
	Token     string
	StartTime time.Time
	stages    []stageEntry
	mu        gosync.Mutex
}

type stageEntry struct {
	Name      string
	StartTime time.Time
	EndTime   time.Time
}

// PerformanceTracker 追踪 TopicDownside proxy data 各阶段性能
type PerformanceTracker struct {
	records gosync.Map // map[token]*PerfRecord
	log     *log.Logger
}

var defaultTracker = NewPerformanceTracker()

// Default 返回全局默认的 PerformanceTracker 实例
func Default() *PerformanceTracker {
	return defaultTracker
}

// NewPerformanceTracker 创建新的性能追踪器
func NewPerformanceTracker() *PerformanceTracker {
	return &PerformanceTracker{
		log: log.With(log.Any("module", "perf_tracker")),
	}
}

// Begin 开始追踪一个新的请求
func (pt *PerformanceTracker) Begin(token string) {
	now := time.Now()
	record := &PerfRecord{
		Token:     token,
		StartTime: now,
		stages:    make([]stageEntry, 0, 12),
	}
	pt.records.Store(token, record)
	pt.log.Debug("perf tracking started", log.Any("token", token))
}

// IsTracking 检查是否正在追踪指定 token
func (pt *PerformanceTracker) IsTracking(token string) bool {
	_, ok := pt.records.Load(token)
	return ok
}

// StartStage 标记某阶段开始
func (pt *PerformanceTracker) StartStage(token, stage string) {
	val, ok := pt.records.Load(token)
	if !ok {
		return
	}
	record := val.(*PerfRecord)
	record.mu.Lock()
	defer record.mu.Unlock()
	record.stages = append(record.stages, stageEntry{
		Name:      stage,
		StartTime: time.Now(),
	})
}

// StartStageAt 用指定时间标记某阶段开始（用于需要追溯的场景）
func (pt *PerformanceTracker) StartStageAt(token, stage string, startTime time.Time) {
	val, ok := pt.records.Load(token)
	if !ok {
		return
	}
	record := val.(*PerfRecord)
	record.mu.Lock()
	defer record.mu.Unlock()
	record.stages = append(record.stages, stageEntry{
		Name:      stage,
		StartTime: startTime,
	})
}

// EndStage 标记某阶段结束
func (pt *PerformanceTracker) EndStage(token, stage string) {
	val, ok := pt.records.Load(token)
	if !ok {
		return
	}
	record := val.(*PerfRecord)
	record.mu.Lock()
	defer record.mu.Unlock()
	for i := len(record.stages) - 1; i >= 0; i-- {
		if record.stages[i].Name == stage && record.stages[i].EndTime.IsZero() {
			record.stages[i].EndTime = time.Now()
			return
		}
	}
}

// TrackStage 便捷方法: 返回一个函数，调用该函数即标记阶段结束
// 用法: defer pt.TrackStage(token, stage)()
func (pt *PerformanceTracker) TrackStage(token, stage string) func() {
	pt.StartStage(token, stage)
	return func() {
		pt.EndStage(token, stage)
	}
}

// Finish 结束追踪并输出性能报告
func (pt *PerformanceTracker) Finish(token string) string {
	val, ok := pt.records.Load(token)
	if !ok {
		return ""
	}
	record := val.(*PerfRecord)
	record.mu.Lock()
	defer record.mu.Unlock()

	totalMs := float64(time.Since(record.StartTime).Nanoseconds()) / 1e6

	// 构建 stages map，将 stage 名称转换为下划线格式并以 _ms 结尾
	stages := make(map[string]float64)
	for _, s := range record.stages {
		if s.EndTime.IsZero() {
			continue
		}
		durMs := float64(s.EndTime.Sub(s.StartTime).Nanoseconds()) / 1e6
		// 将 stage 名称转换为 lowercase 并用下划线替换连字符，最后加上 _ms 后缀
		stageName := s.Name + "_ms"
		stages[stageName] = durMs
	}

	// 构建 JSON 输出
	result := map[string]interface{}{
		"perf_tracker": token,
		"total_ms":     int(totalMs),
		"stages":       stages,
	}

	jsonBytes, err := json.Marshal(result)
	if err != nil {
		pt.log.Error("failed to marshal performance summary", log.Any("error", err))
		pt.records.Delete(token)
		return ""
	}

	report := "performance summary\t" + string(jsonBytes)
	pt.log.Info(report)

	pt.records.Delete(token)
	return report
}

// Summary 获取某个 token 的当前阶段摘要（不结束追踪）
func (pt *PerformanceTracker) Summary(token string) []StageSummary {
	val, ok := pt.records.Load(token)
	if !ok {
		return nil
	}
	record := val.(*PerfRecord)
	record.mu.Lock()
	defer record.mu.Unlock()

	result := make([]StageSummary, 0, len(record.stages))
	for _, s := range record.stages {
		ss := StageSummary{
			Stage:       s.Name,
			StartOffset: float64(s.StartTime.Sub(record.StartTime).Nanoseconds()) / 1e6,
		}
		if !s.EndTime.IsZero() {
			dur := float64(s.EndTime.Sub(s.StartTime).Nanoseconds()) / 1e6
			ss.DurationMs = &dur
		}
		result = append(result, ss)
	}
	return result
}

// StageSummary 阶段耗时摘要
type StageSummary struct {
	Stage       string   `json:"stage"`
	StartOffset float64  `json:"start_offset_ms"`
	DurationMs  *float64 `json:"duration_ms,omitempty"`
}
