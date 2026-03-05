package chain

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	"github.com/baetyl/baetyl-go/v2/pubsub"
	v1 "github.com/baetyl/baetyl-go/v2/spec/v1"
	"github.com/baetyl/baetyl/v2/perf"
	"github.com/baetyl/baetyl/v2/sync"
	utils2 "github.com/baetyl/baetyl/v2/utils"

	"github.com/baetyl/baetyl/v2/ami"
)

func (c *chain) Proxy() error {
	if c.dataCh != nil {
		// Direct channel mode: read from dataCh, write to pipe directly
		c.tomb.Go(c.proxyDataReading)
	} else {
		// Legacy pubsub mode
		c.processor = pubsub.NewProcessor(c.subChan, ProxyTimeout, &chainHandler{chain: c})
		c.processor.Start()
	}

	return c.tomb.Go(c.chainReading, c.proxyConnecting)
}

// proxyDataReading reads messages from the direct data channel and writes to pipe,
// bypassing the pubsub Processor to eliminate serialization overhead.
func (c *chain) proxyDataReading() error {
	pt := perf.Default()
	timer := time.NewTimer(ProxyTimeout)
	defer timer.Stop()

	for {
		select {
		case m, ok := <-c.dataCh:
			if !ok {
				return nil
			}
			timer.Reset(ProxyTimeout)

			pt.StartStage(c.token, perf.StageChainHandlerWrite)

			var cmd []byte
			err := m.Content.Unmarshal(&cmd)
			if err != nil {
				pt.EndStage(c.token, perf.StageChainHandlerWrite)
				c.log.Error("failed to unmarshal data message", log.Error(err))
				continue
			}
			if bytes.Equal([]byte(ExitCmd), cmd) {
				pt.EndStage(c.token, perf.StageChainHandlerWrite)
				c.cancel()
				return nil
			}
			_, err = c.pipe.InWriter.Write(cmd)
			pt.EndStage(c.token, perf.StageChainHandlerWrite)
			if err != nil {
				c.log.Error("failed to write to pipe", log.Error(err))
				return errors.Trace(err)
			}

		case <-timer.C:
			c.log.Warn("proxy data channel timeout")
			c.pb.Publish(c.upside, &v1.Message{
				Kind: v1.MessageData,
				Metadata: map[string]string{
					"success": "false",
					"msg":     "chain timeout",
					"token":   c.token,
				},
			})
			c.pb.Publish(sync.TopicDownside, &v1.Message{
				Kind: v1.MessageCMD,
				Metadata: map[string]string{
					"namespace": c.debugOptions.Namespace,
					"name":      c.debugOptions.Name,
					"container": c.debugOptions.Container,
					"token":     c.token,
					"cmd":       "disconnect",
				},
			})
			return nil

		case <-c.tomb.Dying():
			return nil
		}
	}
}

func (c *chain) proxyConnecting() error {
	defer func() {
		c.log.Debug("connecting close")
		msg := &v1.Message{
			Kind: v1.MessageCMD,
			Metadata: map[string]string{
				"success": "false",
				"msg":     "disconnect",
				"token":   c.token,
			},
		}
		c.pb.Publish(c.upside, msg)
	}()

	var err error
	err = c.RemoteConnection(c.pipe)
	if err != nil {
		c.log.Error("failed to start remote proxy", log.Error(err))
		c.Close()
		return errors.Trace(err)
	}
	return nil
}

// RemoteConnection 建立到目标服务的 TCP 连接并进行双向透传
func (c *chain) RemoteConnection(pipe ami.Pipe) error {
	// 从 metadata 中获取代理目标信息
	schema := c.data["schema"]
	address := c.data["address"]
	port := c.data["port"]

	if address == "" || port == "" || schema == "" {
		return errors.New("proxy target address, port and schema are required")
	}

	targetAddr := fmt.Sprintf("%s:%s", address, port)
	c.log.Info("connecting to proxy target", log.Any("target", targetAddr))

	// 建立到目标服务的连接
	conn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		c.log.Error("failed to connect to target", log.Any("target", targetAddr), log.Error(err))
		return errors.Trace(err)
	}
	c.log.Info("connected to proxy target", log.Any("target", targetAddr))
	defer conn.Close()

	// 检查上下文是否已取消
	select {
	case <-pipe.Ctx.Done():
		c.log.Debug("context cancelled")
		return nil
	default:
	}

	// 启动两个 goroutine 进行双向数据转发
	errChan := make(chan error, 2)
	pt := perf.Default()

	// 从目标服务读取数据，写入管道（发送到云端）
	go func() {
		buf := make([]byte, utils2.ReadBuff)
		for {
			select {
			case <-pipe.Ctx.Done():
				return
			default:
				// [perf] 阶段8: proxy_read_target_write_pipe
				pt.StartStage(c.token, perf.StageProxyReadAndWritePipe)
				n, err := conn.Read(buf)
				if err != nil {
					pt.EndStage(c.token, perf.StageProxyReadAndWritePipe)
					if err != io.EOF {
						c.log.Error("failed to read from target", log.Error(err))
					}
					errChan <- err
					return
				}

				if n > 0 {
					_, err = pipe.OutWriter.Write(buf[:n])
					pt.EndStage(c.token, perf.StageProxyReadAndWritePipe)
					if err != nil {
						c.log.Error("failed to write to pipe", log.Error(err))
						errChan <- err
						return
					}
				} else {
					pt.EndStage(c.token, perf.StageProxyReadAndWritePipe)
				}
			}
		}
	}()

	// 从管道读取数据（来自云端），写入目标服务
	go func() {
		buf := make([]byte, utils2.ReadBuff)
		for {
			select {
			case <-pipe.Ctx.Done():
				return
			default:
				// [perf] 阶段7: proxy_write_to_target
				pt.StartStage(c.token, perf.StageProxyWriteTarget)
				n, err := pipe.InReader.Read(buf)
				if err != nil {
					pt.EndStage(c.token, perf.StageProxyWriteTarget)
					if err != io.EOF {
						c.log.Error("failed to read from pipe", log.Error(err))
					}
					errChan <- err
					return
				}

				if n > 0 {
					_, err = conn.Write(buf[:n])
					pt.EndStage(c.token, perf.StageProxyWriteTarget)
					if err != nil {
						c.log.Error("failed to write to target", log.Error(err))
						errChan <- err
						return
					}
				} else {
					pt.EndStage(c.token, perf.StageProxyWriteTarget)
				}
			}
		}
	}()

	// 等待任一方向出错或上下文取消
	select {
	case <-pipe.Ctx.Done():
		c.log.Debug("proxy connection closed by context")
		return nil
	case err := <-errChan:
		if err != io.EOF {
			c.log.Debug("proxy connection closed", log.Error(err))
		}
		return nil
	}
}
