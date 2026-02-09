// Package webproxy 边端 HTTP 代理服务实现
package webproxy

import (
	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	"github.com/baetyl/baetyl-go/v2/pubsub"
	"github.com/baetyl/baetyl-go/v2/spec/v1"
)

// proxyHandler 处理云端发送的消息
type proxyHandler struct {
	pm  *ProxyManager
	log *log.Logger
}

// newProxyHandler 创建新的代理消息处理器
func newProxyHandler(pm *ProxyManager, logger *log.Logger) pubsub.Handler {
	return &proxyHandler{
		pm:  pm,
		log: logger,
	}
}

// OnMessage 处理收到的消息
func (h *proxyHandler) OnMessage(msg interface{}) error {
	message, ok := msg.(*v1.Message)
	if !ok {
		h.log.Error("invalid message type", log.Any("type", msg))
		return errors.New("invalid message type")
	}

	// 检查是否是代理相关的消息
	if message.Kind != v1.MessageCMD && message.Kind != v1.MessageData {
		return nil
	}

	switch message.Kind {
	case v1.MessageCMD:
		// 处理命令消息
		switch message.Metadata["cmd"] {
		case v1.MessageCommandProxy:
			// 启动代理
			return h.pm.handleProxyCommand(message)
		case v1.MessageCommandDisconnect:
			// 停止代理
			return h.pm.handleDisconnectCommand(message)
		default:
			h.log.Debug("unknown command", log.Any("cmd", message.Metadata["cmd"]))
		}
	case v1.MessageData:
		// 其他下行数据
		return h.pm.handleDownsideData(message)
	}

	return nil
}

// OnTimeout 超时处理
func (h *proxyHandler) OnTimeout() error {
	return nil
}
