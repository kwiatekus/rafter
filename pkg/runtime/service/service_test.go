package service_test

import (
	"context"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kyma-project/rafter/pkg/runtime/service"
	"github.com/onsi/gomega"
)

func TestService_setupHandlers(t *testing.T) {
	for testName, testCase := range map[string]struct {
		endpoints      []service.HTTPEndpoint
		targetEndpoint string
		targetMethod   string
		expectedStatus int
	}{
		"no endpoints": {
			expectedStatus: http.StatusNotFound,
			targetEndpoint: "/test",
			targetMethod:   http.MethodPost,
		},
		"not found": {
			endpoints:      []service.HTTPEndpoint{fixEndpoint("test1", http.StatusOK), fixEndpoint("test2", http.StatusOK), fixEndpoint("test3", http.StatusOK)},
			expectedStatus: http.StatusNotFound,
			targetEndpoint: "/test",
			targetMethod:   http.MethodGet,
		},
		"OK": {
			endpoints:      []service.HTTPEndpoint{fixEndpoint("test", http.StatusOK), fixEndpoint("test2", http.StatusNotFound), fixEndpoint("test3", http.StatusNotFound)},
			expectedStatus: http.StatusOK,
			targetEndpoint: "/test",
			targetMethod:   http.MethodPost,
		},
	} {
		t.Run(testName, func(t *testing.T) {
			// given
			g := gomega.NewWithT(t)
			srv := service.NewTestService(service.Config{})

			for _, endpoint := range testCase.endpoints {
				srv.Register(endpoint)
			}
			recorder := httptest.NewRecorder()

			metRecorder := httptest.NewRecorder()
			mux := srv.SetupHandlers()
			request := httptest.NewRequest(testCase.targetMethod, testCase.targetEndpoint, nil)
			metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)

			// when
			mux.ServeHTTP(recorder, request)

			// then
			g.Expect(recorder.Result().StatusCode).To(gomega.Equal(testCase.expectedStatus))

			// when
			mux.ServeHTTP(metRecorder, metricsReq)

			// then
			resp := metRecorder.Result()
			defer func() {
				err := resp.Body.Close()
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}()
			body, _ := ioutil.ReadAll(resp.Body)
			g.Expect(resp.StatusCode).To(gomega.Equal(http.StatusOK))
			g.Expect(body).To(gomega.Not(gomega.HaveLen(0)))
		})
	}
}

func TestService_Start(t *testing.T) {
	for testName, testCase := range map[string]struct {
		endpoints      []service.HTTPEndpoint
		targetEndpoint string
		targetMethod   string
		expectedStatus int
	}{
		"no endpoints": {
			expectedStatus: http.StatusNotFound,
			targetEndpoint: "/test",
			targetMethod:   http.MethodPost,
		},
		"not found": {
			endpoints:      []service.HTTPEndpoint{fixEndpoint("test1", http.StatusOK), fixEndpoint("test2", http.StatusOK), fixEndpoint("test3", http.StatusOK)},
			expectedStatus: http.StatusNotFound,
			targetEndpoint: "/test",
			targetMethod:   http.MethodGet,
		},
		"OK": {
			endpoints:      []service.HTTPEndpoint{fixEndpoint("test", http.StatusOK), fixEndpoint("test2", http.StatusNotFound), fixEndpoint("test3", http.StatusNotFound)},
			expectedStatus: http.StatusOK,
			targetEndpoint: "/test",
			targetMethod:   http.MethodPost,
		},
	} {
		t.Run(testName, func(t *testing.T) {
			// given
			g := gomega.NewWithT(t)
			srv := service.New(service.Config{})

			for _, endpoint := range testCase.endpoints {
				srv.Register(endpoint)
			}

			ctx, cancel := context.WithCancel(context.TODO())

			// when
			var err error
			wait := sync.WaitGroup{}
			wait.Add(1)
			go func() {
				err = srv.Start(ctx)
				wait.Done()
			}()
			cancel()
			wait.Wait()

			// then
			g.Expect(err).To(gomega.Succeed())

		})
	}
}

var _ service.HTTPEndpoint = &testEndpoint{}

type testEndpoint struct {
	name   string
	status int
}

func (e *testEndpoint) Name() string {
	return e.name
}

func (e *testEndpoint) Handle(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()

	writer.WriteHeader(e.status)
}

func fixEndpoint(name string, status int) *testEndpoint {
	return &testEndpoint{
		name:   name,
		status: status,
	}
}
