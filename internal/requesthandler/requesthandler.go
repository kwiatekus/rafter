package requesthandler

import (
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"github.com/golang/glog"
	"github.com/kyma-project/rafter/internal/bucket"
	"github.com/kyma-project/rafter/internal/fileheader"
	"github.com/kyma-project/rafter/internal/uploader"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type RequestHandler struct {
	client               uploader.MinioClient
	uploadTimeout        time.Duration
	maxUploadWorkers     int
	buckets              bucket.SystemBucketNames
	externalUploadOrigin string
}

type Response struct {
	UploadedFiles []uploader.UploadResult `json:"uploadedFiles,omitempty"`
	Errors        []ResponseError         `json:"errors,omitempty"`
}

type ResponseError struct {
	Message  string `json:"message"`
	FileName string `json:"omitempty,fileName"`
}

var (
	httpServeHistogram = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "rafter_upload_service_http_request_duration_seconds",
		Help: "Requests' duration distribution",
	})
	statusCodesCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rafter_upload_service_http_request_returned_status_code",
		Help: "Service's HTTP response status code",
	}, []string{"status_code"})
)

func incrementStatusCounter(status int) {
	statusCodesCounter.WithLabelValues(strconv.Itoa(status)).Inc()
}

func SetupHandlers(client uploader.MinioClient, buckets bucket.SystemBucketNames, uploadExternalEndpoint string, timeout time.Duration, maxWorkers int) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/v1/upload", New(client, buckets, uploadExternalEndpoint, timeout, maxWorkers))
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}

func New(client uploader.MinioClient, buckets bucket.SystemBucketNames, externalUploadOrigin string, uploadTimeout time.Duration, maxUploadWorkers int) *RequestHandler {
	return &RequestHandler{
		client:               client,
		uploadTimeout:        uploadTimeout,
		maxUploadWorkers:     maxUploadWorkers,
		buckets:              buckets,
		externalUploadOrigin: externalUploadOrigin,
	}
}

func (r *RequestHandler) ServeHTTP(w http.ResponseWriter, rq *http.Request) {
	start := time.Now()

	defer func() {
		err := rq.Body.Close()
		if err != nil {
			glog.Error(errors.Wrap(err, "while closing request body"))
		}
	}()

	err := rq.ParseMultipartForm(32 << 20) // 32MB
	if err != nil {
		wrappedErr := errors.Wrap(err, "while parsing multipart request")
		r.writeInternalError(w, wrappedErr)
		return
	}

	if rq.MultipartForm == nil {
		status := http.StatusBadRequest
		incrementStatusCounter(status)
		r.writeResponse(w, status, Response{
			Errors: []ResponseError{
				{
					Message: "No multipart/form-data form received.",
				},
			},
		})
		return
	}

	defer func() {
		err := rq.MultipartForm.RemoveAll()
		if err != nil {
			glog.Error(errors.Wrap(err, "while removing files loaded from multipart form"))
		}
	}()

	directoryValues := rq.MultipartForm.Value["directory"]

	var directory string
	if directoryValues == nil {
		directory = r.generateDirectoryName()
	} else {
		directory = directoryValues[0]
	}

	privateFiles := rq.MultipartForm.File["private"]
	publicFiles := rq.MultipartForm.File["public"]
	filesCount := len(publicFiles) + len(privateFiles)

	if filesCount == 0 {
		status := http.StatusBadRequest
		incrementStatusCounter(status)
		r.writeResponse(w, http.StatusBadRequest, Response{
			Errors: []ResponseError{
				{
					Message: "No files specified to upload. Use `private` and `public` fields to upload them.",
				},
			},
		})
		return
	}

	u := uploader.New(r.client, r.externalUploadOrigin, r.uploadTimeout, r.maxUploadWorkers)
	fileToUploadCh := r.populateFilesChannel(publicFiles, privateFiles, filesCount, directory)
	uploadedFiles, errs := u.UploadFiles(context.Background(), fileToUploadCh, filesCount)

	glog.Infof("Finished processing request with uploading %d files.", filesCount)

	var uploadErrors []ResponseError
	for _, err := range errs {
		uploadErrors = append(uploadErrors, ResponseError{
			Message:  err.Error.Error(),
			FileName: err.FileName,
		})
	}

	var status int

	if len(uploadErrors) == 0 {
		status = http.StatusOK
		incrementStatusCounter(status)
	} else if len(uploadedFiles) == 0 {
		status = http.StatusBadGateway
		incrementStatusCounter(status)
	} else {
		status = http.StatusMultiStatus
		incrementStatusCounter(status)
	}

	r.writeResponse(w, status, Response{
		UploadedFiles: uploadedFiles,
		Errors:        uploadErrors,
	})

	httpServeHistogram.Observe(time.Since(start).Seconds())
}

func (r *RequestHandler) generateDirectoryName() string {
	unixTime := time.Now().Unix()
	return strconv.FormatInt(unixTime, 32)
}

func (r *RequestHandler) populateFilesChannel(publicFiles, privateFiles []*multipart.FileHeader, filesCount int, directory string) chan uploader.FileUpload {
	filesCh := make(chan uploader.FileUpload, filesCount)

	go func() {
		defer close(filesCh)
		for _, file := range publicFiles {
			filesCh <- uploader.FileUpload{
				Bucket:    r.buckets.Public,
				File:      fileheader.FromMultipart(file),
				Directory: directory,
			}
		}
		for _, file := range privateFiles {
			filesCh <- uploader.FileUpload{
				Bucket:    r.buckets.Private,
				File:      fileheader.FromMultipart(file),
				Directory: directory,
			}
		}
	}()

	return filesCh
}

func (r *RequestHandler) writeResponse(w http.ResponseWriter, statusCode int, resp Response) {
	jsonResponse, err := json.Marshal(resp)
	if err != nil {
		wrappedErr := errors.Wrapf(err, "while marshalling JSON response")
		r.writeInternalError(w, wrappedErr)
	}

	w.Header().Set("Content-Type", "application/json")

	w.WriteHeader(statusCode)
	_, err = w.Write(jsonResponse)
	if err != nil {
		wrappedErr := errors.Wrapf(err, "while writing JSON response")
		glog.Error(wrappedErr)
	}
}

func (r *RequestHandler) writeInternalError(w http.ResponseWriter, err error) {
	r.writeResponse(w, http.StatusInternalServerError, Response{
		Errors: []ResponseError{
			{Message: err.Error()},
		},
	})
}
