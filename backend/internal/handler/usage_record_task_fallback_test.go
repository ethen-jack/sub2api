package handler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖：worker 池已停止（进程关停窗口）时，计费任务不得静默丢失，
// 必须降级为内联同步执行；显式配置的 drop/sample 溢出丢弃仍按配置语义保留。

func newStoppedUsageRecordPoolForTest() *service.UsageRecordWorkerPool {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:    1,
		QueueSize:      1,
		TaskTimeout:    time.Second,
		OverflowPolicy: config.UsageRecordOverflowPolicySync,
	})
	pool.Stop()
	return pool
}

func TestGatewayHandlerSubmitUsageRecordTask_StoppedPoolFallsBackToSync(t *testing.T) {
	h := &GatewayHandler{usageRecordWorkerPool: newStoppedUsageRecordPoolForTest()}

	executed := false
	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		executed = true
	})
	require.True(t, executed, "池已停止时计费任务必须内联同步执行")
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_StoppedPoolFallsBackToSync(t *testing.T) {
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: newStoppedUsageRecordPoolForTest()}

	executed := false
	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		executed = true
	})
	require.True(t, executed, "池已停止时计费任务必须内联同步执行")
}

func TestGatewayHandlerSubmitUsageRecordTask_DropPolicyOverflowStillDrops(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:    1,
		QueueSize:      1,
		TaskTimeout:    time.Minute,
		OverflowPolicy: config.UsageRecordOverflowPolicyDrop,
	})
	t.Cleanup(pool.Stop)
	h := &GatewayHandler{usageRecordWorkerPool: pool}

	started := make(chan struct{})
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	// 占满 worker 槽位后再填满队列，保证第三个任务触发溢出。
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(ctx context.Context) {
		close(started)
		<-block
	}))
	<-started
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(ctx context.Context) {
		<-block
	}))

	var executed atomic.Bool
	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		executed.Store(true)
	})
	time.Sleep(50 * time.Millisecond)
	require.False(t, executed.Load(), "drop 溢出策略是运维显式配置的取舍，不应被同步兜底覆盖")
}

func TestGatewayHandlerInterruptedUsageSurvivesQueueOverflow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     *service.ForwardResult
		forwardErr error
		wantCalls  int32
	}{
		{
			name:       "upstream interruption with observed usage",
			result:     &service.ForwardResult{Stream: true, Usage: service.ClaudeUsage{InputTokens: 4, OutputTokens: 1}},
			forwardErr: errors.New("stream usage incomplete: missing terminal event"),
			wantCalls:  1,
		},
		{
			name:      "estimated output without final usage",
			result:    &service.ForwardResult{Stream: true, Usage: service.ClaudeUsage{OutputTokens: 25, OutputTokensEstimated: true}},
			wantCalls: 1,
		},
		{
			name:      "client disconnected but upstream usage drained",
			result:    &service.ForwardResult{Stream: true, ClientDisconnect: true, Usage: service.ClaudeUsage{OutputTokens: 25}},
			wantCalls: 1,
		},
		{
			name:   "ordinary successful request retains configured drop policy",
			result: &service.ForwardResult{Stream: true, Usage: service.ClaudeUsage{OutputTokens: 25}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			// Registered first: verify again after the queue has drained and
			// stopped, so a synchronous fallback cannot also execute later.
			t.Cleanup(func() { require.Equal(t, tc.wantCalls, calls.Load()) })
			pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
				WorkerCount: 1, QueueSize: 1, TaskTimeout: time.Second,
				OverflowPolicy: config.UsageRecordOverflowPolicyDrop,
			})
			t.Cleanup(pool.Stop)
			blocked := make(chan struct{})
			started := make(chan struct{})
			t.Cleanup(func() { close(blocked) })
			require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
				close(started)
				<-blocked
			}))
			<-started
			require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) { <-blocked }))
			h := &GatewayHandler{usageRecordWorkerPool: pool}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			h.submitForwardUsageRecordTask(ctx, tc.result, tc.forwardErr, func(billingCtx context.Context) {
				require.NoError(t, billingCtx.Err(), "client cancellation must not cancel billing")
				calls.Add(1)
			})
			require.Equal(t, tc.wantCalls, calls.Load())
		})
	}
}
