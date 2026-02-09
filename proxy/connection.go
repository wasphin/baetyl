// Package webproxy 边端 HTTP 代理服务实现
package webproxy

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/baetyl/baetyl-go/v2/log"
	"github.com/baetyl/baetyl-go/v2/spec/v1"
	v2sync "github.com/baetyl/baetyl/v2/sync"
)

// proxyConnection 代理连接
type proxyConnection struct {
	token   string
	schema  string
	address string
	port    int16

	// 到目标服务的连接
	targetConn net.Conn

	// 代理管理器
	pm *ProxyManager

	// 最后活动时间
	lastActive time.Time
	mutex      sync.RWMutex

	log *log.Logger
}

// newProxyConnection 创建新的代理连接
func newProxyConnection(token string, schema string, address string, port int16, targetConn net.Conn, pm *ProxyManager, logger *log.Logger) *proxyConnection {
	return &proxyConnection{
		token:      token,
		schema:     schema,
		address:    address,
		port:       port,
		targetConn: targetConn,
		pm:         pm,
		lastActive: time.Now(),
		log:        logger.With(log.Any("token", token)),
	}
}

// forwardToCloud 从目标服务读取数据并发送到云端
func (pc *proxyConnection) forwardToCloud() {
	defer func() {
		if r := recover(); r != nil {
			pc.log.Error("panic in forwardToCloud", log.Any("panic", r))
		}
		pc.Close()
	}()

	buf := make([]byte, 32*1024) // 32KB 缓冲区

	for {
		// 从目标连接读取数据
		n, err := pc.targetConn.Read(buf)
		if err != nil {
			if err != io.EOF {
				pc.log.Error("failed to read from target", log.Error(err))
			}
			return
		}

		// 更新最后活动时间
		pc.updateLastActive()

		// 发送数据到云端
		data := make([]byte, n)
		copy(data, buf[:n])

		msg := &v1.Message{
			Kind: v1.MessageData,
			Metadata: map[string]string{
				"token": pc.token,
			},
			Content: v1.LazyValue{Value: data},
		}

		// 发送到云端的上行 topic
		if err := pc.pm.pb.Publish(v2sync.TopicUpside, msg); err != nil {
			pc.log.Error("failed to send data to cloud", log.Error(err))
			return
		}

		pc.log.Debug("sent data to cloud", log.Any("size", n))
	}
}

// writeToTarget 将云端的数据写入目标连接
func (pc *proxyConnection) writeToTarget(data []byte) error {
	pc.mutex.Lock()
	defer pc.mutex.Unlock()

	// 更新最后活动时间
	pc.updateLastActive()

	// 写入目标连接
	n, err := pc.targetConn.Write(data)
	if err != nil {
		pc.log.Error("failed to write to target", log.Error(err))
		return err
	}

	if n != len(data) {
		pc.log.Warn("partial write", log.Any("expected", len(data)), log.Any("actual", n))
	}

	pc.log.Debug("wrote data to target", log.Any("size", n))
	return nil
}

// updateLastActive 更新最后活动时间
func (pc *proxyConnection) updateLastActive() {
	pc.mutex.Lock()
	defer pc.mutex.Unlock()
	pc.lastActive = time.Now()
}

// isIdle 检查连接是否空闲
func (pc *proxyConnection) isIdle(timeout time.Duration) bool {
	pc.mutex.RLock()
	defer pc.mutex.RUnlock()
	return time.Since(pc.lastActive) > timeout
}

// Close 关闭代理连接
func (pc *proxyConnection) Close() error {
	pc.log.Info("closing proxy connection")

	// 从管理器中移除
	pc.pm.removeConnection(pc.token)

	// 关闭目标连接
	if pc.targetConn != nil {
		return pc.targetConn.Close()
	}

	return nil
}
