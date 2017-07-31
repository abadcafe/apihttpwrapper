package apihttpwrapper

import (
	"github.com/julienschmidt/httprouter"
	"io"
	"net/http"
)

type methodCallLogger struct {
	row *AccessLogRow
}

const ServiceHandlerAccessLogRowFillerContextKey = "apihttpwrapper.ServiceHandlerAccessLogRowFiller"

func (l *methodCallLogger) Record(field string, value string) {
	l.row.SetRowField(field, value)
}

func ServiceHandlerAccessLogRowFillerFactory(row *AccessLogRow) AccessLogRowFiller {
	return &methodCallLogger{row}
}

type Route struct {
	Method            string
	Path              string
	Function          interface{}
	BypassRequestBody bool
}

func RegisterFunctionsToHTTPRouter(r *httprouter.Router, loggerContextKey interface{}, routes []*Route) error {
	for _, rt := range routes {
		handler, err := NewServiceHandler(rt.Function, loggerContextKey, rt.BypassRequestBody)
		if err != nil {
			return err
		}

		r.Handle(rt.Method, rt.Path, handler.ServeHTTPWithParams)
	}

	return nil
}

func NewHTTPRouter(routes []*Route) (*httprouter.Router, error) {
	router := httprouter.New()
	err := RegisterFunctionsToHTTPRouter(router, ServiceHandlerAccessLogRowFillerContextKey, routes)
	if err != nil {
		return nil, err
	}

	return router, nil
}

func NewLoggingHTTPRouter(routes []*Route, loggingHeaders []string, logWriter io.Writer) (http.Handler, error) {
	router, err := NewHTTPRouter(routes)
	if err != nil {
		return nil, err
	}

	return NewAccessLogDecorator(router, logWriter, loggingHeaders, ServiceHandlerAccessLogRowFillerContextKey,
		ServiceHandlerAccessLogRowFillerFactory), nil
}
