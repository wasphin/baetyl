package utils

import (
	"github.com/baetyl/baetyl-go/v2/errors"
	v1 "github.com/baetyl/baetyl-go/v2/spec/v1"
	"github.com/baetyl/baetyl-go/v2/utils"

	"github.com/baetyl/baetyl/v2/plugin"
)

func UnwrapEnv(msg *v1.Message) error {
	data, err := utils.ParseEnv(msg.Content.GetJSON())
	if err != nil {
		return errors.Trace(err)
	}
	msg.Content.SetJSON(data)
	return nil
}

// RequestWithEnvUnwrapped unwraps the env in response
func RequestWithEnvUnwrapped(link plugin.Link, req *v1.Message) (*v1.Message, error) {
	res, err := link.Request(req)
	if err != nil {
		return nil, errors.Trace(err)
	}
	err = UnwrapEnv(res)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return res, nil
}
