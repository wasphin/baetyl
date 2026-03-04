package chain

import (
	"fmt"
	"io"
	"net"

	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	"github.com/baetyl/baetyl-go/v2/pubsub"
	v1 "github.com/baetyl/baetyl-go/v2/spec/v1"

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

	// 从目标服务读取数据，写入管道（发送到云端）
	go func() {
		buf := make([]byte, 16*1024) // 16KB 缓冲区
		for {
			select {
			case <-pipe.Ctx.Done():
				return
			default:
				n, err := conn.Read(buf)
				if err != nil {
					if err != io.EOF {
						c.log.Error("failed to read from target", log.Error(err))
					}
					errChan <- err
					return
				}

				if n > 0 {
					c.log.Info("read data from target", log.Any("n", n))
					c.log.Debug("read data from target", log.Any("data", string(buf[:n])))
					_, err = pipe.OutWriter.Write(buf[:n])
					if err != nil {
						c.log.Error("failed to write to pipe", log.Error(err))
						errChan <- err
						return
					}
				}
			}
		}
	}()

	// 从管道读取数据（来自云端），写入目标服务
	go func() {
		buf := make([]byte, 16*1024) // 16KB 缓冲区
		for {
			select {
			case <-pipe.Ctx.Done():
				return
			default:
				n, err := pipe.InReader.Read(buf)
				if err != nil {
					if err != io.EOF {
						c.log.Error("failed to read from pipe", log.Error(err))
					}
					errChan <- err
					return
				}

				if n > 0 {
					c.log.Info("write data to target", log.Any("n", n))
					c.log.Debug("write data to target", log.Any("data", string(buf[:n])))
					_, err = conn.Write(buf[:n])
					if err != nil {
						c.log.Error("failed to write to target", log.Error(err))
						errChan <- err
						return
					}
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
