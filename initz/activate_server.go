package initz

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"os"
	"runtime/debug"

	"github.com/baetyl/baetyl-go/v2/comctx"
	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	"github.com/baetyl/baetyl-go/v2/utils"
)

const (
	KeyBaetylSyncAddr = "BAETYL_SYNC_ADDR"
)

func (active *Activate) startServer() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", active.handleView)
	mux.HandleFunc("/update", handleWrapper(http.MethodPost, active.handleUpdate))
	mux.HandleFunc("/active", handleWrapper(http.MethodPost, active.handleActive))
	srv := &http.Server{}
	srv.Handler = mux
	srv.Addr = active.cfg.Init.Active.Collector.Server.Listen
	active.srv = srv
	return errors.Trace(active.srv.ListenAndServe())
}

func (active *Activate) closeServer() {
	err := active.srv.Shutdown(context.Background())
	if err != nil {
		active.log.Error("active", log.Any("server err", err))
	}
}

func (active *Activate) handleView(w http.ResponseWriter, _ *http.Request) {
	attrs := map[string]interface{}{
		"Attributes": active.cfg.Init.Active.Collector.Attributes,
		"Nodeinfo":   active.cfg.Init.Active.Collector.NodeInfo,
		"Serial":     active.cfg.Init.Active.Collector.Serial,
	}
	tpl, err := template.ParseFiles(active.cfg.Init.Active.Collector.Server.Pages + "/active.html.template")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = tpl.Execute(w, attrs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (active *Activate) handleActiveImpl(req *http.Request) error {
	err := req.ParseForm()
	if err != nil {
		return comctx.Error(comctx.ErrRequestParamInvalid, comctx.Field("err", err))
	}
	attributes := make(map[string]string)
	for _, attr := range active.cfg.Init.Active.Collector.Attributes {
		val := req.Form.Get(attr.Name)
		if val == "" {
			attributes[attr.Name] = attr.Value
		} else {
			attributes[attr.Name] = val
		}
	}
	for _, ni := range active.cfg.Init.Active.Collector.NodeInfo {
		val := req.Form.Get(ni.Name)
		attributes[ni.Name] = val
	}
	for _, si := range active.cfg.Init.Active.Collector.Serial {
		val := req.Form.Get(si.Name)
		attributes[si.Name] = val
	}
	active.log.Info("active", log.Any("server attrs", attributes))
	active.attrs = attributes

	if batchName, ok := attributes["batch"]; ok {
		active.batch.name = batchName
	}
	if ns, ok := attributes["namespace"]; ok {
		active.batch.namespace = ns
	}
	if initAddr, ok := attributes["initAddr"]; ok {
		active.cfg.Init.Active.Address = initAddr

	}
	if syncAddr, ok := attributes["syncAddr"]; ok {
		_ = os.Setenv(KeyBaetylSyncAddr, syncAddr)
	}
	return active.activate()
}

func (active *Activate) handleUpdate(w http.ResponseWriter, req *http.Request) error {
	err := active.handleActiveImpl(req)
	if e := err.(errors.Coder); e.Code() == comctx.ErrRequestParamInvalid {
		return err
	}

	var tpl *template.Template
	page := "/success.html.template"
	if !utils.FileExists(active.cfg.Node.Cert) {
		page = "/failed.html.template"
	}
	tpl, err = template.ParseFiles(active.cfg.Init.Active.Collector.Server.Pages + page)
	if err != nil {
		return comctx.Error(comctx.ErrUnknown, comctx.Field("error", err))
	}
	err = tpl.Execute(w, nil)
	if err != nil {
		return comctx.Error(comctx.ErrUnknown, comctx.Field("error", err))
	}

	return nil
}

func (active *Activate) handleActive(w http.ResponseWriter, req *http.Request) error {
	if err := active.handleActiveImpl(req); err != nil {
		return err
	}
	_, _ = w.Write([]byte("active success"))
	return nil
}

type HandlerFunc func(w http.ResponseWriter, req *http.Request) error

// 如果 handler 返回 error, 则由 wrapper 统一封装 json response, 否则默认 handler 已经生成回复信息
func handleWrapper(method string, handler HandlerFunc) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if r := recover(); r != nil {
				err, ok := r.(error)
				if !ok {
					err = comctx.Error(comctx.ErrUnknown, comctx.Field("error", r))
				}
				log.L().Info("handle a panic", log.Code(err), log.Error(err), log.Any("panic", string(debug.Stack())))
				populateFailedResponse(w, err)
			}
		}()

		if req.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := handler(w, req); err != nil {
			populateFailedResponse(w, err)
		}
	}
}

func populateFailedResponse(w http.ResponseWriter, err error) {
	var code string
	switch e := err.(type) {
	case errors.Coder:
		code = e.Code()
	default:
		code = comctx.ErrUnknown
	}

	log.L().Error("process failed.", log.Code(err))

	body := map[string]interface{}{
		"code":    code,
		"message": err.Error(),
	}
	bytes, _ := json.Marshal(body)

	w.WriteHeader(comctx.Code(code).ToHTTPStatus())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(bytes)
}
