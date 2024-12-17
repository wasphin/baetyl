package initz

import (
	"context"
	goerrors "errors"
	"html/template"
	"net/http"
	"os"

	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	"github.com/baetyl/baetyl-go/v2/utils"
)

const (
	KeyBaetylSyncAddr = "BAETYL_SYNC_ADDR"
)

var (
	errMethodNotAllowed = errors.New("method not allowed")
	errBadRequest       = errors.New("bad request")
	errForbidden        = errors.New("Forbidden")
)

func (active *Activate) startServer() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", active.handleView)
	mux.HandleFunc("/update", active.handleUpdate)
	mux.HandleFunc("/active", active.handleActive)
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

func (active *Activate) handleView(w http.ResponseWriter, req *http.Request) {
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
	if req.Method != http.MethodPost {
		return errMethodNotAllowed
	}
	err := req.ParseForm()
	if err != nil {
		return errBadRequest
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

func (active *Activate) handleUpdate(w http.ResponseWriter, req *http.Request) {
	err := active.handleActiveImpl(req)
	switch {
	case goerrors.Is(err, errMethodNotAllowed):
		http.Error(w, "post only", http.StatusMethodNotAllowed)
		return
	case goerrors.Is(err, errBadRequest):
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	default:
	}

	var tpl *template.Template
	page := "/success.html.template"
	if !utils.FileExists(active.cfg.Node.Cert) {
		page = "/failed.html.template"
	}
	tpl, err = template.ParseFiles(active.cfg.Init.Active.Collector.Server.Pages + page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = tpl.Execute(w, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (active *Activate) handleActive(w http.ResponseWriter, req *http.Request) {
	err := active.handleActiveImpl(req)
	switch {
	case goerrors.Is(err, errMethodNotAllowed):
		http.Error(w, "post only", http.StatusMethodNotAllowed)
	case goerrors.Is(err, errBadRequest):
		http.Error(w, "bad request", http.StatusBadRequest)
	case goerrors.Is(err, errForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		_, _ = w.Write([]byte("active success"))
	}
}
