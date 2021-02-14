package apihttpwrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/schema"
	"github.com/julienschmidt/httprouter"
	"golang.org/x/net/trace"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

type ServiceMethodContext struct {
	Context              context.Context
	RemoteAddr           string
	RequestHeader        http.Header
	RequestBodyReader    io.ReadCloser
	ResponseStatusSetter func(status int)
	ResponseHeader       http.Header
	ResponseBodyWriter   io.Writer
}

type MethodLogger interface {
	Record(field string, value string)
}

type ServiceHandler struct {
	loggerContextKey   interface{}
	method             *serviceMethod
	bypassRequestBody  bool
}

type FormattedResponse struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data"`
}

type serviceMethod struct {
	value   reflect.Value
	argType reflect.Type
}

type panicStack struct {
	Panic string `json:"panic"`
	Stack string `json:"stack"`
}

const traceFamily = "apihttpwrapper.ServiceHandler"

var formDecoder = schema.NewDecoder()

func init() {
	formDecoder.IgnoreUnknownKeys(true)
}

func isTypeServiceMethodContext(methodType reflect.Type) bool {
	return methodType.Kind() == reflect.Ptr && methodType.Elem().Kind() == reflect.Struct &&
		methodType.Elem().Name() == "ServiceMethodContext"
}

func isStructPointer(methodType reflect.Type) bool {
	return methodType.Kind() == reflect.Ptr && methodType.Elem().Kind() == reflect.Struct
}

func isStringMap(methodType reflect.Type) bool {
	return methodType.Kind() == reflect.Map && methodType.Key().Kind() == reflect.String &&
		methodType.Elem().Kind() == reflect.Interface && methodType.Elem().Name() == ""
}

func isSlice(methodType reflect.Type) bool {
	return methodType.Kind() == reflect.Slice
}

func isCustomResponseBodyFunction(methodType reflect.Type) bool {
	return methodType.NumOut() == 1 &&
		methodType.Out(0).Kind() == reflect.Interface && methodType.Out(0).Name() == "error"
}

func isDelegatedResponseBodyFunction(methodType reflect.Type) bool {
	return methodType.NumOut() == 2 &&
		methodType.Out(0).Kind() == reflect.Ptr && methodType.Out(0).Elem().Kind() == reflect.Struct &&
		methodType.Out(1).Kind() == reflect.Interface && methodType.Out(1).Name() == "error"
}

func checkServiceMethodPrototype(methodType reflect.Type) error {
	if methodType == nil || methodType.Kind() != reflect.Func {
		return fmt.Errorf("you should provide a function or object method")
	}

	if methodType.NumIn() != 2 {
		return fmt.Errorf("the service method should have two arguments")
	}

	if !isTypeServiceMethodContext(methodType.In(0)) {
		return fmt.Errorf("the first argument should be type *ServiceMethodContext")
	}

	if !isSlice(methodType.In(1)) && !isStringMap(methodType.In(1)) && !isStructPointer(methodType.In(1)) {
		return fmt.Errorf("the second argument should be a struct pointer, slice or map[string]interface{}")
	}

	if !isCustomResponseBodyFunction(methodType) && !isDelegatedResponseBodyFunction(methodType) {
		return fmt.Errorf("the service method only can return error interface or (*struct, error)")
	}

	return nil
}

func NewServiceHandler(method interface{}, loggerContextKey interface{},
	bypassRequestBody bool) (h *ServiceHandler, err error) {
	// the method prototype like this: 'func(*ServiceMethodContext, *struct) (anything)'
	methodType := reflect.TypeOf(method)
	err = checkServiceMethodPrototype(methodType)
	if err != nil {
		return
	}

	h = &ServiceHandler{
		loggerContextKey: loggerContextKey,
		method: &serviceMethod{
			value:   reflect.ValueOf(method),
			argType: methodType.In(1),
		},
		bypassRequestBody: bypassRequestBody,
	}

	return
}

func setResponseHeader(w http.ResponseWriter) {
	// Prevents Internet Explorer from MIME-sniffing a response away from the declared content-type
	w.Header().Set("x-content-type-options", "nosniff")
	w.Header().Set("Content-Type", "application/json")
}

func writeResponse(w http.ResponseWriter, tr trace.Trace, data interface{}) {
	tr.LazyPrintf("%+v", data)
	setResponseHeader(w)
	_ = json.NewEncoder(w).Encode(data)
}

func writeErrorResponse(w http.ResponseWriter, tr trace.Trace, resp *FormattedResponse) {
	tr.LazyPrintf("%s: %+v", resp.Msg, resp.Data)
	if resp.Code >= 400 {
		tr.SetError()
	}

	setResponseHeader(w)
	w.WriteHeader(resp.Code)
	_ = json.NewEncoder(w).Encode(resp)
}

func doServiceMethodCall(method *serviceMethod, in []reflect.Value) (out []reflect.Value, ps *panicStack) {
	defer func() {
		if panicInfo := recover(); panicInfo != nil {
			ps = &panicStack{
				Panic: fmt.Sprintf("%s", panicInfo),
				Stack: fmt.Sprintf("%s", debug.Stack()),
			}
		}
	}()

	out = method.value.Call(in)
	return
}

func (h *ServiceHandler) parseArgument(r *http.Request, params httprouter.Params, arg interface{}) error {
	method := strings.ToUpper(r.Method)
	contentType := strings.ToLower(r.Header.Get("Content-Type"))

	// query string has lowest priority.
	var err error
	if method == "POST" && contentType == "multipart/form-data" {
		err = r.ParseMultipartForm(1024)
	} else {
		err = r.ParseForm()
	}

	if err != nil {
		return err
	}

	err = formDecoder.Decode(arg, r.Form)
	if err != nil {
		return err
	}

	// json content's priority is higher than query string, but lower than params in url pattern.
	if method == "POST" && !h.bypassRequestBody && strings.HasPrefix(contentType, "application/json") {
		err = json.NewDecoder(r.Body).Decode(arg)
		if err != nil {
			return err
		}
	}

	// params in the url pattern has highest priority.
	if params != nil {
		paramValues := url.Values{}
		for _, param := range params {
			paramValues.Set(param.Key, param.Value)
		}

		err = formDecoder.Decode(arg, paramValues)
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *ServiceHandler) ServeHTTP(respWriter http.ResponseWriter, req *http.Request) {
	h.ServeHTTPWithParams(respWriter, req, nil)
}

func (h *ServiceHandler) ServeHTTPWithParams(rw http.ResponseWriter, r *http.Request, params httprouter.Params) {
	tracer := trace.New(traceFamily, r.URL.Path)
	defer tracer.Finish()

	// extract arguments.
	arg := reflect.New(h.method.argType.Elem())
	err := h.parseArgument(r, params, arg.Interface())
	if err != nil {
		writeErrorResponse(rw, tracer, &FormattedResponse{400, "parse argument failed", err.Error()})
		return
	}

	// do method call.
	beginTime := time.Now()

	respStatus := http.StatusOK
	out, methodPanic := doServiceMethodCall(h.method, []reflect.Value{
		reflect.ValueOf(&ServiceMethodContext{
			Context:              r.Context(),
			RemoteAddr:           r.RemoteAddr,
			RequestHeader:        r.Header,
			RequestBodyReader:    r.Body,
			ResponseStatusSetter: func(status int) {
				respStatus = status
				rw.WriteHeader(status)
			},
			ResponseHeader:       rw.Header(),
			ResponseBodyWriter:   rw,
		}),
		arg,
	})

	duration := time.Now().Sub(beginTime)

	var methodError error
	var methodReturn interface{}
	var respData interface{}

	if methodPanic != nil {
		respData = &FormattedResponse{500, "service method panicked", methodPanic}
		writeErrorResponse(rw, tracer, respData.(*FormattedResponse))
	} else if len(out) == 2 {
		methodReturn = out[0].Interface()
		if out[1].Interface() != nil {
			methodError = out[1].Interface().(error)
		}
	} else if len(out) == 1 {
		if out[0].Interface() != nil {
			methodError = out[0].Interface().(error)
		}
	} else {
		// the method prototype have more than one return value, it is forbidden.
		panic(fmt.Sprintf("return values error: %+v", out))
	}

	if methodError != nil {
		if respStatus == http.StatusOK {
			respStatus = 500
		}

		respData = &FormattedResponse{respStatus, "service method error", methodError.Error()}
		writeErrorResponse(rw, tracer, respData.(*FormattedResponse))
	} else if methodReturn != nil {
		respData = methodReturn
		writeResponse(rw, tracer, methodReturn)
	}

	// record some thing if logger existed.
	if h.loggerContextKey == nil {
		return
	}

	v := r.Context().Value(h.loggerContextKey)
	if v == nil {
		return
	}

	logger, ok := v.(MethodLogger)
	if !ok {
		return
	}

	marshaledArgs, err := json.Marshal(arg.Interface())
	if err != nil {
		panic(err)
	}

	marshaledData, err := json.Marshal(respData)
	if err != nil {
		panic(err)
	}

	logger.Record("args", string(marshaledArgs))
	logger.Record("resp", string(marshaledData))
	logger.Record("methodBegin", beginTime.Format("2006-01-02 15:04:05.999999999"))
	logger.Record("methodDuration", strconv.FormatFloat(duration.Seconds(), 'f', -1, 64))
}
