package apihttpwrapper

import (
	"context"
	"errors"
	"github.com/julienschmidt/httprouter"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type dummyMethodLogger struct{}

var dummyLogger = &dummyMethodLogger{}

func (l *dummyMethodLogger) Record(_ string, _ string) {}

func TestCheckServiceMethodPrototype(t *testing.T) {
	t.Run("not function", func(t *testing.T) {
		err := checkServiceMethodPrototype(reflect.TypeOf(1))
		if err == nil {
			t.Error()
		}
	})

	t.Run("arguments count wrong", func(t *testing.T) {
		err := checkServiceMethodPrototype(reflect.TypeOf(
			func() {},
		))
		if err == nil {
			t.Error()
		}
	})

	t.Run("first argument type wrong", func(t *testing.T) {
		err := checkServiceMethodPrototype(reflect.TypeOf(
			func(*struct{}, *struct{}) {},
		))
		if err == nil {
			t.Error()
		}
	})

	t.Run("second argument type wrong", func(t *testing.T) {
		err := checkServiceMethodPrototype(reflect.TypeOf(
			func(*ServiceMethodContext, struct{}) {},
		))
		if err == nil {
			t.Error()
		}
	})

	t.Run("return values type wrong", func(t *testing.T) {
		err := checkServiceMethodPrototype(reflect.TypeOf(
			func(*ServiceMethodContext, *struct{}) (int, int, int) { return 0, 0, 0 },
		))
		if err == nil {
			t.Error()
		}
	})

	t.Run("normal method", func(t *testing.T) {
		err := checkServiceMethodPrototype(reflect.TypeOf(
			func(*ServiceMethodContext, *struct{}) error { return nil },
		))
		if err != nil {
			t.Error(err)
		}

		err = checkServiceMethodPrototype(reflect.TypeOf(
			func(*ServiceMethodContext, *struct{}) (*struct{}, error) { return nil, nil },
		))
		if err != nil {
			t.Error(err)
		}
	})
}

type testingRequest struct {
	method            string
	uri               string
	body              string
	header            map[string]string
	params            httprouter.Params
	bypassRequestBody bool
	expectStatus      int
}

func doTest(t *testing.T, tr *testingRequest, fun interface{}) {
	if tr.expectStatus == 0 {
		tr.expectStatus = 200
	}

	if tr.method == "" {
		tr.method = "POST"
	}

	if tr.uri == "" {
		tr.uri = "/"
	}

	r := httptest.NewRequest(tr.method, tr.uri, strings.NewReader(tr.body))
	r = r.WithContext(context.WithValue(r.Context(), "aw_test", dummyLogger))
	if tr.header != nil {
		for k, v := range tr.header {
			r.Header.Add(k, v)
		}
	}

	h, err := NewServiceHandler(fun, "aw_test", tr.bypassRequestBody)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTPWithParams(recorder, r, tr.params)
	if recorder.Code != tr.expectStatus {
		t.Errorf("expected code is %d, but response code is %d. the body is below: \n%s", tr.expectStatus,
			recorder.Code, recorder.Body)
	}
}

func TestServeHTTP(t *testing.T) {
	t.Run("normal request", func(t *testing.T) {
		doTest(
			t,
			&testingRequest{
				method: "POST",
				uri:    "/",
				body:   "{}",
				header: map[string]string{"content-type": "application/json"},
			},
			func(*ServiceMethodContext, *struct{}) (*struct{ A int }, error) {
				return &struct{ A int }{A: 1}, nil
			},
		)
	})

	t.Run("normal request and function return nil without error", func(t *testing.T) {
		doTest(
			t,
			&testingRequest{
				body:   "{}",
				header: map[string]string{"content-type": "application/json"},
			},
			func(*ServiceMethodContext, *struct{}) (*struct{ A int }, error) {
				return nil, nil
			},
		)
	})

	t.Run("normal request with arguments in path, body and query", func(t *testing.T) {
		doTest(
			t,
			&testingRequest{
				uri:    "/?A=1&B=1&C=1",
				body:   "{\"B\":2,\"C\":2}",
				params: []httprouter.Param{{Key: "C", Value: "3"}},
				header: map[string]string{"content-type": "application/json"},
			},
			func(_ *ServiceMethodContext, args *struct{ A, B, C int }) (*struct{ A int }, error) {
				if args.A != 1 || args.B != 2 || args.C != 3 {
					t.Error()
				}
				return nil, nil
			},
		)
	})

	t.Run("normal request while bypassed request body", func(t *testing.T) {
		doTest(
			t,
			&testingRequest{
				uri:               "/?A=1&B=1&C=1",
				body:              "{\"B\":2,\"C\":2}",
				params:            []httprouter.Param{{Key: "C", Value: "3"}},
				header:            map[string]string{"content-type": "application/json"},
				bypassRequestBody: true,
			},
			func(_ *ServiceMethodContext, args *struct{ A, B, C int }) (*struct{ A int }, error) {
				if args.A != 1 || args.B != 1 || args.C != 3 {
					t.Error()
				}
				return nil, nil
			},
		)
	})

	t.Run("normal request while content-type hasn't specified", func(t *testing.T) {
		doTest(
			t,
			&testingRequest{
				uri:    "/?A=1&B=1&C=1",
				body:   "{\"B\":2,\"C\":2}",
				params: []httprouter.Param{{Key: "C", Value: "3"}},
			},
			func(_ *ServiceMethodContext, args *struct{ A, B, C int }) (*struct{ A int }, error) {
				if args.A != 1 || args.B != 1 || args.C != 3 {
					t.Error()
				}
				return nil, nil
			},
		)
	})

	t.Run("json syntax error", func(t *testing.T) {
		doTest(
			t,
			&testingRequest{
				body:         "{1234}",
				header:       map[string]string{"content-type": "application/json"},
				expectStatus: 400,
			},
			func(*ServiceMethodContext, *struct{}) (*struct{ A int }, error) {
				return nil, nil
			},
		)
	})

	t.Run("empty request body and hasn't bypassed body", func(t *testing.T) {
		doTest(
			t,
			&testingRequest{
				header:       map[string]string{"content-type": "application/json"},
				expectStatus: 400,
			},
			func(*ServiceMethodContext, *struct{}) (*struct{ A int }, error) {
				return nil, nil
			},
		)
	})

	t.Run("function return error", func(t *testing.T) {
		doTest(
			t,
			&testingRequest{
				body:         "{}",
				header:       map[string]string{"content-type": "application/json"},
				expectStatus: 500,
			},
			func(*ServiceMethodContext, *struct{}) (*struct{ A int }, error) {
				return nil, errors.New("expected error")
			},
		)
	})

	t.Run("function panicked", func(t *testing.T) {
		doTest(
			t,
			&testingRequest{
				body:         "{}",
				header:       map[string]string{"content-type": "application/json"},
				expectStatus: 500,
			},
			func(*ServiceMethodContext, *struct{}) (*struct{ A int }, error) {
				panic("expected panic")
				return nil, nil
			},
		)
	})
}
