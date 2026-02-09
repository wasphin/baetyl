// Package webproxy 边端 HTTP 代理服务实现
package webproxy

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/baetyl/baetyl-go/v2/context"
	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	"github.com/baetyl/baetyl-go/v2/pubsub"
	"github.com/baetyl/baetyl-go/v2/spec/v1"
	"github.com/baetyl/baetyl/v2/plugin"
	v2sync "github.com/baetyl/baetyl/v2/sync"
)

const (
	// 连接空闲超时时间
	connIdleTimeout = 5 * time.Minute
	TopicProxy      = "proxy"
)

var (
	ErrInvalidToken = errors.New("invalid token")
)

// ProxyManager 管理所有代理连接
type ProxyManager struct {
	// token -> proxyConnection 映射
	connections sync.Map
	// pubsub 服务，用于发送消息到云端
	pb plugin.Pubsub
	// 消息处理器
	handler pubsub.Handler
	// 消息处理器实例
	processor pubsub.Processor
	// 日志
	log *log.Logger
	// 上下文
	ctx context.Context
}

// NewProxyManager 创建新的代理管理器
func NewProxyManager(ctx context.Context, pb plugin.Pubsub) (*ProxyManager, error) {
	logger := log.With(log.Any("module", "webproxy"))

	pm := &ProxyManager{
		pb:  pb,
		log: logger,
		ctx: ctx,
	}

	pm.handler = newProxyHandler(pm, logger)

	// 订阅云端发送的代理命令和下行数据
	// 消息由 sync.dispatch 分发到 TopicWebProxy
	ch, err := pb.Subscribe(TopicProxy)
	if err != nil {
		return nil, errors.Trace(err)
	}

	pm.processor = pubsub.NewProcessor(ch, 0, pm.handler)
	return pm, nil
}

// Start 启动代理管理器
func (pm *ProxyManager) Start() error {
	pm.log.Info("starting proxy manager")
	pm.processor.Start()

	// 启动空闲连接清理协程
	go pm.cleanupIdleConnections()

	return nil
}

// Close 关闭代理管理器
func (pm *ProxyManager) Close() error {
	pm.log.Info("closing proxy manager")
	pm.processor.Close()

	// 关闭所有连接
	pm.connections.Range(func(key, value interface{}) bool {
		conn := value.(*proxyConnection)
		_ = conn.Close()
		return true
	})

	return nil
}

// handleProxyCommand 处理代理连接命令
func (pm *ProxyManager) handleProxyCommand(msg *v1.Message) error {
	token := msg.Metadata["token"]
	if token == "" {
		return errors.Trace(ErrInvalidToken)
	}

	var req v1.ProxyRequest
	if err := msg.Content.Unmarshal(&req); err != nil {
		pm.log.Error("failed to unmarshal proxy request", log.Error(err))
		return errors.Trace(err)
	}

	pm.log.Info("received proxy command",
		log.Any("token", token),
		log.Any("schema", req.Schema),
		log.Any("address", req.Address),
		log.Any("port", req.Port))

	// 创建到目标服务的连接
	targetAddr := fmt.Sprintf("%s:%d", req.Address, req.Port)
	conn, err := net.DialTimeout(req.Schema, targetAddr, 10*time.Second)
	if err != nil {
		pm.log.Error("failed to connect to target",
			log.Any("target", targetAddr),
			log.Error(err))
		// 返回失败响应
		return pm.sendProxyResponse(token, false, err.Error())
	}

	// 创建代理连接，token 确保唯一
	proxyConn := newProxyConnection(token, req.Schema, req.Address, req.Port, conn, pm, pm.log)
	pm.connections.Store(token, proxyConn)

	// 启动数据转发协程
	go proxyConn.forwardToCloud()

	// 返回成功响应
	return pm.sendProxyResponse(token, true, "")
}

// handleDisconnectCommand 处理断开连接命令
func (pm *ProxyManager) handleDisconnectCommand(msg *v1.Message) error {
	token := msg.Metadata["token"]
	if token == "" {
		return errors.Trace(ErrInvalidToken)
	}

	pm.log.Info("received disconnect command", log.Any("token", token))

	value, ok := pm.connections.Load(token)
	if !ok {
		pm.log.Warn("connection not found", log.Any("token", token))
		return nil
	}

	conn := value.(*proxyConnection)
	_ = conn.Close()
	pm.connections.Delete(token)

	return nil
}

// handleDownsideData 处理云端发送的下行数据
func (pm *ProxyManager) handleDownsideData(msg *v1.Message) error {
	token := msg.Metadata["token"]
	if token == "" {
		return errors.Trace(ErrInvalidToken)
	}

	value, ok := pm.connections.Load(token)
	if !ok {
		pm.log.Warn("connection not found for data", log.Any("token", token))
		return nil
	}

	var data []byte
	if err := msg.Content.Unmarshal(&data); err != nil {
		pm.log.Error("failed to unmarshal data", log.Error(err))
		return errors.Trace(err)
	}

	conn := value.(*proxyConnection)
	return conn.writeToTarget(data)
}

// sendProxyResponse 发送代理连接响应
func (pm *ProxyManager) sendProxyResponse(token string, success bool, errMsg string) error {
	resp := &v1.Message{
		Kind: v1.MessageResponse,
		Metadata: map[string]string{
			"token":   token,
			"success": fmt.Sprintf("%t", success),
			"msg":     errMsg,
		},
		Content: v1.LazyValue{Value: errMsg},
	}
	return pm.pb.Publish(v2sync.TopicUpside, resp)
}

// removeConnection 移除连接
func (pm *ProxyManager) removeConnection(token string) {
	pm.connections.Delete(token)
}

// cleanupIdleConnections 清理空闲连接
func (pm *ProxyManager) cleanupIdleConnections() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pm.connections.Range(func(key, value interface{}) bool {
				conn := value.(*proxyConnection)
				if conn.isIdle(connIdleTimeout) {
					pm.log.Info("closing idle connection", log.Any("token", key))
					_ = conn.Close()
				}
				return true
			})
		case <-pm.ctx.WaitChan():
			return
		}
	}
}
