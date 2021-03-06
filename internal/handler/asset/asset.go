package asset

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyma-project/rafter/internal/assethook"
	"github.com/kyma-project/rafter/internal/loader"
	"github.com/kyma-project/rafter/internal/store"
	"github.com/kyma-project/rafter/pkg/apis/rafter/v1beta1"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
)

type Handler interface {
	Do(ctx context.Context, now time.Time, instance MetaAccessor, spec v1beta1.CommonAssetSpec, status v1beta1.CommonAssetStatus) (*v1beta1.CommonAssetStatus, error)
}

type MetaAccessor interface {
	GetNamespace() string
	GetName() string
	GetGeneration() int64
	GetDeletionTimestamp() *v1.Time
	GetFinalizers() []string
	SetFinalizers(finalizers []string)
	GetObjectKind() schema.ObjectKind
	DeepCopyObject() runtime.Object
}

var _ Handler = &assetHandler{}

type FindBucketStatus func(ctx context.Context, namespace, name string) (*v1beta1.CommonBucketStatus, bool, error)

type assetHandler struct {
	recorder          record.EventRecorder
	findBucketStatus  FindBucketStatus
	store             store.Store
	loader            loader.Loader
	validator         assethook.Validator
	mutator           assethook.Mutator
	metadataExtractor assethook.MetadataExtractor
	log               logr.Logger
	relistInterval    time.Duration
}

func New(log logr.Logger, recorder record.EventRecorder, store store.Store, loader loader.Loader, findBucketFnc FindBucketStatus, validator assethook.Validator, mutator assethook.Mutator, metadataExtractor assethook.MetadataExtractor, relistInterval time.Duration) Handler {
	return &assetHandler{
		recorder:          recorder,
		store:             store,
		loader:            loader,
		findBucketStatus:  findBucketFnc,
		validator:         validator,
		mutator:           mutator,
		metadataExtractor: metadataExtractor,
		log:               log,
		relistInterval:    relistInterval,
	}
}

func (h *assetHandler) Do(ctx context.Context, now time.Time, instance MetaAccessor, spec v1beta1.CommonAssetSpec, status v1beta1.CommonAssetStatus) (*v1beta1.CommonAssetStatus, error) {
	h.logInfof("Start common Asset handling")
	defer h.logInfof("Finish common Asset handling")

	switch {
	case h.isOnDelete(instance):
		h.logInfof("On delete")
		return h.onDelete(ctx, instance, spec)
	case h.isOnAddOrUpdate(instance, status):
		h.logInfof("On add or update")
		return h.getStatus(instance, v1beta1.AssetPending, v1beta1.AssetScheduled), nil
	case h.isOnReady(status, now):
		h.logInfof("On ready")
		return h.onReady(ctx, instance, spec, status)
	case h.isOnPending(status, now):
		h.logInfof("On pending")
		return h.onPending(ctx, instance, spec, status)
	case h.isOnFailed(status):
		h.logInfof("On failed")
		return h.onPending(ctx, instance, spec, status)
	default:
		h.logInfof("Action not taken")
		return nil, nil
	}
}

func (*assetHandler) isOnAddOrUpdate(object MetaAccessor, status v1beta1.CommonAssetStatus) bool {
	return status.ObservedGeneration != object.GetGeneration()
}

func (h *assetHandler) isOnPending(status v1beta1.CommonAssetStatus, now time.Time) bool {
	if status.Phase == v1beta1.AssetPending {
		if status.Reason == v1beta1.AssetBucketNotReady && now.Before(status.LastHeartbeatTime.Add(h.relistInterval)) {
			return false
		}

		return true
	}

	return false
}

func (*assetHandler) isOnDelete(object MetaAccessor) bool {
	return !object.GetDeletionTimestamp().IsZero()
}

func (*assetHandler) isOnFailed(status v1beta1.CommonAssetStatus) bool {
	return status.Phase == v1beta1.AssetFailed &&
		status.Reason != v1beta1.AssetValidationFailed &&
		status.Reason != v1beta1.AssetMutationFailed
}

func (h *assetHandler) isOnReady(status v1beta1.CommonAssetStatus, now time.Time) bool {
	return status.Phase == v1beta1.AssetReady && now.After(status.LastHeartbeatTime.Add(h.relistInterval))
}

func (h *assetHandler) onDelete(ctx context.Context, object MetaAccessor, spec v1beta1.CommonAssetSpec) (*v1beta1.CommonAssetStatus, error) {
	h.logInfof("Deleting Asset")
	bucketStatus, isReady, err := h.findBucketStatus(ctx, object.GetNamespace(), spec.BucketRef.Name)
	if err != nil {
		return nil, errors.Wrap(err, "while reading bucket status")
	}
	if !isReady {
		h.logInfof("Nothing to delete, bucket %s is not ready", spec.BucketRef.Name)
		return nil, nil
	}

	if err := h.deleteRemoteContent(ctx, object, bucketStatus.RemoteName); err != nil {
		return nil, err
	}
	h.logInfof("Asset deleted")

	return nil, nil
}

func (h *assetHandler) deleteRemoteContent(ctx context.Context, object MetaAccessor, bucketName string) error {
	h.logInfof("Checking if bucket contains files for asset")
	prefix := object.GetName()
	files, err := h.store.ListObjects(ctx, bucketName, prefix)
	if err != nil {
		return errors.Wrap(err, "while listing files in bucket")
	}

	if len(files) == 0 {
		h.logInfof("Bucket doesn't contains asset files, nothing to delete")
		return nil
	}

	h.logInfof("Deleting asset remote content")
	if err := h.store.DeleteObjects(ctx, bucketName, prefix); err != nil {
		return errors.Wrap(err, "while deleting asset content")
	}
	h.logInfof("Remote content deleted")
	h.recordNormalEventf(object, v1beta1.AssetCleaned)

	return nil
}

func (h *assetHandler) onReady(ctx context.Context, object MetaAccessor, spec v1beta1.CommonAssetSpec, status v1beta1.CommonAssetStatus) (*v1beta1.CommonAssetStatus, error) {
	h.logInfof("Checking if bucket %s is ready", spec.BucketRef.Name)
	bucketStatus, isReady, err := h.findBucketStatus(ctx, object.GetNamespace(), spec.BucketRef.Name)
	if err != nil {
		h.recordWarningEventf(object, v1beta1.AssetBucketError, err.Error())
		return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetBucketError, err.Error()), err
	}
	if !isReady {
		h.logInfof("Bucket %s is not ready", spec.BucketRef.Name)
		h.recordWarningEventf(object, v1beta1.AssetBucketNotReady)
		return h.getStatus(object, v1beta1.AssetPending, v1beta1.AssetBucketNotReady), nil
	}
	h.logInfof("Bucket %s is ready", spec.BucketRef.Name)

	h.logInfof("Checking if store contains all files")
	exists, err := h.store.ContainsAllObjects(ctx, bucketStatus.RemoteName, object.GetName(), h.extractNames(status.AssetRef.Files))
	if err != nil {
		h.recordWarningEventf(object, v1beta1.AssetRemoteContentVerificationError, err.Error())
		return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetRemoteContentVerificationError, err.Error()), err
	}
	if !exists {
		h.recordWarningEventf(object, v1beta1.AssetMissingContent)
		return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetMissingContent), err
	}

	h.logInfof("Asset is up-to-date")

	return h.getReadyStatus(object, status.AssetRef.BaseURL, status.AssetRef.Files, v1beta1.AssetUploaded), nil
}

func (h *assetHandler) extractNames(files []v1beta1.AssetFile) []string {
	names := make([]string, 0, len(files))

	for _, file := range files {
		names = append(names, file.Name)
	}

	return names
}

func (h *assetHandler) onPending(ctx context.Context, object MetaAccessor, spec v1beta1.CommonAssetSpec, status v1beta1.CommonAssetStatus) (*v1beta1.CommonAssetStatus, error) {
	h.logInfof("Checking if bucket %s is ready", spec.BucketRef.Name)
	bucketStatus, isReady, err := h.findBucketStatus(ctx, object.GetNamespace(), spec.BucketRef.Name)
	if err != nil {
		h.recordWarningEventf(object, v1beta1.AssetBucketError, err.Error())
		return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetBucketError, err.Error()), err
	}
	if !isReady {
		h.logInfof("Bucket %s is not ready", spec.BucketRef.Name)
		h.recordWarningEventf(object, v1beta1.AssetBucketNotReady)
		return h.getStatus(object, v1beta1.AssetPending, v1beta1.AssetBucketNotReady), nil
	}
	h.logInfof("Bucket %s is ready", spec.BucketRef.Name)

	if err := h.deleteRemoteContent(ctx, object, bucketStatus.RemoteName); err != nil {
		h.recordWarningEventf(object, v1beta1.AssetCleanupError, err.Error())
		return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetCleanupError, err.Error()), err
	}

	h.logInfof("Loading files from %s", spec.Source.URL)
	basePath, filenames, err := h.loader.Load(spec.Source.URL, object.GetName(), spec.Source.Mode, spec.Source.Filter)
	defer h.loader.Clean(basePath)
	if err != nil {
		h.recordWarningEventf(object, v1beta1.AssetPullingFailed, err.Error())
		return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetPullingFailed, err.Error()), err
	}
	h.logInfof("Files loaded")
	h.recordNormalEventf(object, v1beta1.AssetPulled)

	if len(spec.Source.MutationWebhookService) > 0 {
		h.logInfof("Mutating Asset content")
		result, err := h.mutator.Mutate(ctx, basePath, filenames, spec.Source.MutationWebhookService)
		if err != nil {
			h.recordWarningEventf(object, v1beta1.AssetMutationFailed, err.Error())
			return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetMutationError, err.Error()), err
		}
		if !result.Success {
			h.recordWarningEventf(object, v1beta1.AssetMutationFailed, result.Messages)
			return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetMutationFailed, result.Messages), nil
		}
		h.logInfof("Asset content mutated")
		h.recordNormalEventf(object, v1beta1.AssetMutated)
	}

	if len(spec.Source.ValidationWebhookService) > 0 {
		h.logInfof("Validating Asset content")
		result, err := h.validator.Validate(ctx, basePath, filenames, spec.Source.ValidationWebhookService)
		if err != nil {
			h.recordWarningEventf(object, v1beta1.AssetValidationError, err.Error())
			return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetValidationError, err.Error()), err
		}
		if !result.Success {
			h.recordWarningEventf(object, v1beta1.AssetValidationFailed, result.Messages)
			return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetValidationFailed, result.Messages), nil
		}
		h.logInfof("Asset content validated")
		h.recordNormalEventf(object, v1beta1.AssetValidated)
	}

	files := h.populateFiles(filenames)
	if len(spec.Source.MetadataWebhookService) > 0 {
		h.logInfof("Extracting metadata from Assets content")
		result, err := h.metadataExtractor.Extract(ctx, basePath, filenames, spec.Source.MetadataWebhookService)
		if err != nil {
			h.recordWarningEventf(object, v1beta1.AssetMetadataExtractionFailed, err.Error())
			return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetMetadataExtractionFailed, err.Error()), err
		}

		files = h.mergeMetadata(files, result)

		h.logInfof("Metadata extracted")
		h.recordNormalEventf(object, v1beta1.AssetMetadataExtracted)
	}

	h.logInfof("Uploading Asset content to Minio")
	if err := h.store.PutObjects(ctx, bucketStatus.RemoteName, object.GetName(), basePath, filenames); err != nil {
		h.recordWarningEventf(object, v1beta1.AssetUploadFailed, err.Error())
		return h.getStatus(object, v1beta1.AssetFailed, v1beta1.AssetUploadFailed, err.Error()), err
	}
	h.logInfof("Asset content uploaded")
	h.recordNormalEventf(object, v1beta1.AssetUploaded)

	return h.getReadyStatus(object, h.getBaseUrl(bucketStatus.URL, object.GetName()), files, v1beta1.AssetUploaded), nil
}

func (h *assetHandler) populateFiles(filenames []string) []v1beta1.AssetFile {
	result := make([]v1beta1.AssetFile, 0, len(filenames))

	for _, filename := range filenames {
		result = append(result, v1beta1.AssetFile{Name: filename})
	}

	return result
}

func (h *assetHandler) mergeMetadata(files []v1beta1.AssetFile, metadatas []assethook.File) []v1beta1.AssetFile {
	metadataMap := make(map[string]*json.RawMessage)
	for _, metadata := range metadatas {
		metadataMap[metadata.Name] = metadata.Metadata
	}

	result := make([]v1beta1.AssetFile, 0, len(files))
	for _, file := range files {
		metadata := h.toRawExtension(metadataMap[file.Name])

		result = append(result, v1beta1.AssetFile{Name: file.Name, Metadata: metadata})
	}

	return result
}

func (h *assetHandler) toRawExtension(message *json.RawMessage) *runtime.RawExtension {
	if message == nil {
		return nil
	}

	return &runtime.RawExtension{Raw: *message}
}

func (h *assetHandler) getBaseUrl(bucketUrl, assetName string) string {
	return fmt.Sprintf("%s/%s", bucketUrl, assetName)
}

func (h *assetHandler) recordNormalEventf(object MetaAccessor, reason v1beta1.AssetReason, args ...interface{}) {
	h.recordEventf(object, "Normal", reason, args...)
}

func (h *assetHandler) recordWarningEventf(object MetaAccessor, reason v1beta1.AssetReason, args ...interface{}) {
	h.recordEventf(object, "Warning", reason, args...)
}

func (h *assetHandler) logInfof(message string, args ...interface{}) {
	h.log.Info(fmt.Sprintf(message, args...))
}

func (h *assetHandler) recordEventf(object MetaAccessor, eventType string, reason v1beta1.AssetReason, args ...interface{}) {
	h.recorder.Eventf(object, eventType, reason.String(), reason.Message(), args...)
}

func (h *assetHandler) getReadyStatus(object MetaAccessor, baseUrl string, files []v1beta1.AssetFile, reason v1beta1.AssetReason, args ...interface{}) *v1beta1.CommonAssetStatus {
	status := h.getStatus(object, v1beta1.AssetReady, reason, args...)
	status.AssetRef.BaseURL = baseUrl
	status.AssetRef.Files = files
	return status
}

func (*assetHandler) getStatus(object MetaAccessor, phase v1beta1.AssetPhase, reason v1beta1.AssetReason, args ...interface{}) *v1beta1.CommonAssetStatus {
	return &v1beta1.CommonAssetStatus{
		LastHeartbeatTime:  v1.Now(),
		ObservedGeneration: object.GetGeneration(),
		Phase:              phase,
		Reason:             reason,
		Message:            fmt.Sprintf(reason.Message(), args...),
	}
}
