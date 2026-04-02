package sync

import (
	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	v1 "github.com/baetyl/baetyl-go/v2/spec/v1"

	"github.com/baetyl/baetyl/v2/plugin"
)

var (
	ErrProcessorToManyMessages = errors.New("too many messages")
)

type handler struct {
	link plugin.Link
	log  *log.Logger
	sem  chan struct{}
}

func (h *handler) OnMessage(msg interface{}) error {
	m := msg.(*v1.Message)

	enableAsync := m.Kind == v1.MessageCMD

	if enableAsync {
		select {
		case h.sem <- struct{}{}:
			go func(m *v1.Message) {
				defer func() { <-h.sem }()

				if err := h.link.Send(m); err != nil {
					h.log.Error("failed to send message to link", log.Error(err))
				}
			}(m)
			return nil
		default:
			h.log.Warn("failed to handle message", log.Error(ErrProcessorToManyMessages))
			return nil
		}
	}

	return h.link.Send(m)
}

func (h *handler) OnTimeout() error {
	msg := &v1.Message{
		Kind: v1.MessageData,
		Metadata: map[string]string{
			"success": "false",
			"msg":     "sync timeout",
		},
	}
	return h.link.Send(msg)
}
