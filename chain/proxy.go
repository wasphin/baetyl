package chain

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	"github.com/baetyl/baetyl-go/v2/pubsub"
	v1 "github.com/baetyl/baetyl-go/v2/spec/v1"
	"github.com/baetyl/baetyl/v2/perf"
	utils2 "github.com/baetyl/baetyl/v2/utils"

	"github.com/baetyl/baetyl/v2/ami"
)

func (c *chain) Proxy() error {
	c.processor = pubsub.NewProcessor(c.subChan, ProxyTimeout, &chainHandler{chain: c})
	c.processor.Start()

	return c.tomb.Go(c.chainReading, c.proxyConnecting)
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

	// 设置 TCP 连接参数
	tcpConn := conn.(*net.TCPConn)
	tcpConn.SetReadBuffer(64 * 1024)  // 64KB 读缓冲区
	tcpConn.SetWriteBuffer(64 * 1024) // 64KB 写缓冲区

	// 使用缓冲 Writer 写入管道
	pipeWriter := bufio.NewWriterSize(pipe.OutWriter, 32*1024) // 32KB 缓冲

	// 从目标服务读取数据，写入管道（发送到云端）
	go func() {
		buf := make([]byte, utils2.ReadBuff)
		defer pipeWriter.Flush() // 确保退出时刷新缓冲区

		for {
			select {
			case <-pipe.Ctx.Done():
				return
			default:
				// [perf] 阶段8: proxy_read_target_write_pipe
				pt.StartStage(c.token, perf.StageProxyReadAndWritePipe)

				// 设置读超时，避免无限阻塞
				if err := tcpConn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
					c.log.Warn("failed to set read deadline", log.Error(err))
				}

				n, err := conn.Read(buf)
				if err != nil {
					pt.EndStage(c.token, perf.StageProxyReadAndWritePipe)

					// 判断是否为超时错误
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						c.log.Warn("read from target timeout (30s), may be idle connection")
						return // 超时退出，避免资源泄漏
					}

					if err != io.EOF {
						c.log.Error("failed to read from target", log.Error(err))
					}
					errChan <- err
					return
				}

				// 清除读超时
				tcpConn.SetReadDeadline(time.Time{})

				if n > 0 {
					// 使用缓冲 Writer 写入管道
					_, err = pipeWriter.Write(buf[:n])
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

	// 使用缓冲 Writer 减少系统调用
	writer := bufio.NewWriterSize(tcpConn, 32*1024) // 32KB 应用层缓冲

	// 从管道读取数据（来自云端），写入目标服务
	go func() {
		buf := make([]byte, utils2.ReadBuff)
		defer writer.Flush() // 确保退出时刷新缓冲区

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
					// 设置写超时，避免无限阻塞
					if err := tcpConn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
						c.log.Warn("failed to set write deadline", log.Error(err))
					}

					// 使用缓冲 Writer 写入
					_, err = writer.Write(buf[:n])
					pt.EndStage(c.token, perf.StageProxyWriteTarget)
					if err != nil {
						// 判断是否为超时错误
						if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
							c.log.Error("write to target timeout (5s)", log.Error(err))
						} else {
							c.log.Error("failed to write to target", log.Error(err))
						}
						errChan <- err
						return
					}

					// 清除写超时
					tcpConn.SetWriteDeadline(time.Time{})
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
